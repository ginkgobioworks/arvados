class Node < ArvadosModel
  include HasUuid
  include KindAndEtag
  include CommonApiTemplate
  serialize :info, Hash
  serialize :properties, Hash
  before_validation :ensure_ping_secret
  after_update :dns_server_update

  # Only a controller can figure out whether or not the current API tokens
  # have access to the associated Job.  They're expected to set
  # job_readable=true if the Job UUID can be included in the API response.
  belongs_to(:job, foreign_key: :job_uuid, primary_key: :uuid)
  attr_accessor :job_readable

  api_accessible :user, :extend => :common do |t|
    t.add :hostname
    t.add :domain
    t.add :ip_address
    t.add :last_ping_at
    t.add :slot_number
    t.add :status
    t.add :api_job_uuid, as: :job_uuid
    t.add :crunch_worker_state
    t.add :properties
  end
  api_accessible :superuser, :extend => :user do |t|
    t.add :first_ping_at
    t.add :info
    t.add lambda { |x| Rails.configuration.compute_node_nameservers }, :as => :nameservers
  end

  def domain
    super || Rails.configuration.compute_node_domain
  end

  def api_job_uuid
    job_readable ? job_uuid : nil
  end

  def crunch_worker_state
    return 'down' if slot_number.nil?
    case self.info.andand['slurm_state']
    when 'alloc', 'comp'
      'busy'
    when 'idle'
      'idle'
    else
      'down'
    end
  end

  def status
    if !self.last_ping_at
      if db_current_time - self.created_at > 5.minutes
        'startup-fail'
      else
        'pending'
      end
    elsif db_current_time - self.last_ping_at > 1.hours
      'missing'
    else
      'running'
    end
  end

  def ping(o)
    raise "must have :ip and :ping_secret" unless o[:ip] and o[:ping_secret]

    if o[:ping_secret] != self.info['ping_secret']
      logger.info "Ping: secret mismatch: received \"#{o[:ping_secret]}\" != \"#{self.info['ping_secret']}\""
      raise ArvadosModel::UnauthorizedError.new("Incorrect ping_secret")
    end

    current_time = db_current_time
    self.last_ping_at = current_time

    @bypass_arvados_authorization = true

    # Record IP address
    if self.ip_address.nil?
      logger.info "#{self.uuid} ip_address= #{o[:ip]}"
      self.ip_address = o[:ip]
      self.first_ping_at = current_time
    end

    # Record instance ID if not already known
    if o[:ec2_instance_id]
      if !self.info['ec2_instance_id']
        self.info['ec2_instance_id'] = o[:ec2_instance_id]
      elsif self.info['ec2_instance_id'] != o[:ec2_instance_id]
        logger.debug "Multiple nodes have credentials for #{self.uuid}"
        raise "#{self.uuid} is already running at #{self.info['ec2_instance_id']} so rejecting ping from #{o[:ec2_instance_id]}"
      end
    end

    # Assign slot_number
    if self.slot_number.nil?
      try_slot = 0
      begin
        self.slot_number = try_slot
        begin
          self.save!
          break
        rescue ActiveRecord::RecordNotUnique
          try_slot += 1
        end
        raise "No available node slots" if try_slot == Rails.configuration.max_compute_nodes
      end while true
    end

    # Assign hostname
    if self.hostname.nil? and Rails.configuration.assign_node_hostname
      self.hostname = self.class.hostname_for_slot(self.slot_number)
    end

    # Record other basic stats
    ['total_cpu_cores', 'total_ram_mb', 'total_scratch_mb'].each do |key|
      if value = (o[key] or o[key.to_sym])
        self.properties[key] = value.to_i
      else
        self.properties.delete(key)
      end
    end

    save!
  end

  protected

  def ensure_ping_secret
    self.info['ping_secret'] ||= rand(2**256).to_s(36)
  end

  def dns_server_update
    if self.hostname_changed? or self.ip_address_changed?
      if not self.ip_address.nil?
        stale_conflicting_nodes = Node.where('id != ? and ip_address = ? and last_ping_at < ?',self.id,self.ip_address,10.minutes.ago)
        if not stale_conflicting_nodes.empty?
          # One or more stale compute node records have the same IP address as the new node.
          # Clear the ip_address field on the stale nodes.
          stale_conflicting_nodes.each do |stale_node|
            stale_node.ip_address = nil
            stale_node.save!
          end
        end
      end
      if self.hostname and self.ip_address
        self.class.dns_server_update(self.hostname, self.ip_address)
      end
    end
  end

  def self.dns_server_update hostname, ip_address
    ok = true

    ptr_domain = ip_address.
      split('.').reverse.join('.').concat('.in-addr.arpa')

    template_vars = {
      hostname: hostname,
      uuid_prefix: Rails.configuration.uuid_prefix,
      ip_address: ip_address,
      ptr_domain: ptr_domain,
    }

    if Rails.configuration.dns_server_conf_dir and Rails.configuration.dns_server_conf_template
      begin
        begin
          template = IO.read(Rails.configuration.dns_server_conf_template)
        rescue => e
          logger.error "Reading #{Rails.configuration.dns_server_conf_template}: #{e.message}"
          raise
        end

        hostfile = File.join Rails.configuration.dns_server_conf_dir, "#{hostname}.conf"
        File.open hostfile+'.tmp', 'w' do |f|
          f.puts template % template_vars
        end
        File.rename hostfile+'.tmp', hostfile
      rescue => e
        logger.error "Writing #{hostfile}: #{e.message}"
        ok = false
      end
    end

    if Rails.configuration.dns_server_update_command
      cmd = Rails.configuration.dns_server_update_command % template_vars
      if not system cmd
        logger.error "dns_server_update_command #{cmd.inspect} failed: #{$?}"
        ok = false
      end
    end

    if Rails.configuration.dns_server_conf_dir and Rails.configuration.dns_server_reload_command
      restartfile = File.join(Rails.configuration.dns_server_conf_dir, 'restart.txt')
      begin
        File.open(restartfile, 'w') do |f|
          # Typically, this is used to trigger a dns server restart
          f.puts Rails.configuration.dns_server_reload_command
        end
      rescue => e
        logger.error "Unable to write #{restartfile}: #{e.message}"
        ok = false
      end
    end

    ok
  end

  def self.hostname_for_slot(slot_number)
    config = Rails.configuration.assign_node_hostname

    return nil if !config

    sprintf(config, {:slot_number => slot_number})
  end

  # At startup, make sure all DNS entries exist.  Otherwise, slurmctld
  # will refuse to start.
  if Rails.configuration.dns_server_conf_dir and Rails.configuration.dns_server_conf_template and Rails.configuration.assign_node_hostname
    (0..Rails.configuration.max_compute_nodes-1).each do |slot_number|
      hostname = hostname_for_slot(slot_number)
      hostfile = File.join Rails.configuration.dns_server_conf_dir, "#{hostname}.conf"
      if !File.exists? hostfile
        n = Node.where(:slot_number => slot_number).first
        if n.nil? or n.ip_address.nil?
          dns_server_update(hostname, '127.40.4.0')
        else
          dns_server_update(hostname, n.ip_address)
        end
      end
    end
  end

  def permission_to_update
    @bypass_arvados_authorization or super
  end

  def permission_to_create
    current_user and current_user.is_admin
  end
end
