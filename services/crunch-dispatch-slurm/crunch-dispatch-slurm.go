package main

// Dispatcher service for Crunch that submits containers to the slurm queue.

import (
	"encoding/json"
	"flag"
	"fmt"
	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/dispatch"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Config used by crunch-dispatch-slurm
type Config struct {
	SbatchArguments []string
	PollPeriod      arvados.Duration

	// crunch-run command to invoke. The container UUID will be
	// appended. If nil, []string{"crunch-run"} will be used.
	//
	// Example: []string{"crunch-run", "--cgroup-parent-subsystem=memory"}
	CrunchRunCommand []string
}

func main() {
	err := doMain()
	if err != nil {
		log.Fatalf("%q", err)
	}
}

var (
	config        Config
	squeueUpdater Squeue
)

const defaultConfigPath = "/etc/arvados/crunch-dispatch-slurm/config.json"

func doMain() error {
	flags := flag.NewFlagSet("crunch-dispatch-slurm", flag.ExitOnError)
	flags.Usage = func() { usage(flags) }

	configPath := flags.String(
		"config",
		defaultConfigPath,
		"`path` to json configuration file")

	// Parse args; omit the first arg which is the command name
	flags.Parse(os.Args[1:])

	err := readConfig(&config, *configPath)
	if err != nil {
		log.Printf("Error reading configuration: %v", err)
		return err
	}

	if config.CrunchRunCommand == nil {
		config.CrunchRunCommand = []string{"crunch-run"}
	}

	if config.PollPeriod == 0 {
		config.PollPeriod = arvados.Duration(10 * time.Second)
	}

	arv, err := arvadosclient.MakeArvadosClient()
	if err != nil {
		log.Printf("Error making Arvados client: %v", err)
		return err
	}
	arv.Retries = 25

	squeueUpdater.StartMonitor(time.Duration(config.PollPeriod))
	defer squeueUpdater.Done()

	dispatcher := dispatch.Dispatcher{
		Arv:            arv,
		RunContainer:   run,
		PollInterval:   time.Duration(config.PollPeriod),
		DoneProcessing: make(chan struct{})}

	err = dispatcher.RunDispatcher()
	if err != nil {
		return err
	}

	return nil
}

// sbatchCmd
func sbatchFunc(container arvados.Container) *exec.Cmd {
	memPerCPU := math.Ceil(float64(container.RuntimeConstraints.RAM) / (float64(container.RuntimeConstraints.VCPUs) * 1048576))

	var sbatchArgs []string
	sbatchArgs = append(sbatchArgs, "--share")
	sbatchArgs = append(sbatchArgs, config.SbatchArguments...)
	sbatchArgs = append(sbatchArgs, fmt.Sprintf("--job-name=%s", container.UUID))
	sbatchArgs = append(sbatchArgs, fmt.Sprintf("--mem-per-cpu=%d", int(memPerCPU)))
	sbatchArgs = append(sbatchArgs, fmt.Sprintf("--cpus-per-task=%d", container.RuntimeConstraints.VCPUs))

	return exec.Command("sbatch", sbatchArgs...)
}

// scancelCmd
func scancelFunc(container arvados.Container) *exec.Cmd {
	return exec.Command("scancel", "--name="+container.UUID)
}

// Wrap these so that they can be overridden by tests
var sbatchCmd = sbatchFunc
var scancelCmd = scancelFunc

// Submit job to slurm using sbatch.
func submit(dispatcher *dispatch.Dispatcher,
	container arvados.Container, crunchRunCommand []string) (submitErr error) {
	defer func() {
		// If we didn't get as far as submitting a slurm job,
		// unlock the container and return it to the queue.
		if submitErr == nil {
			// OK, no cleanup needed
			return
		}
		err := dispatcher.Arv.Update("containers", container.UUID,
			arvadosclient.Dict{
				"container": arvadosclient.Dict{"state": "Queued"}},
			nil)
		if err != nil {
			log.Printf("Error unlocking container %s: %v", container.UUID, err)
		}
	}()

	// Create the command and attach to stdin/stdout
	cmd := sbatchCmd(container)
	stdinWriter, stdinerr := cmd.StdinPipe()
	if stdinerr != nil {
		submitErr = fmt.Errorf("Error creating stdin pipe %v: %q", container.UUID, stdinerr)
		return
	}

	stdoutReader, stdoutErr := cmd.StdoutPipe()
	if stdoutErr != nil {
		submitErr = fmt.Errorf("Error creating stdout pipe %v: %q", container.UUID, stdoutErr)
		return
	}

	stderrReader, stderrErr := cmd.StderrPipe()
	if stderrErr != nil {
		submitErr = fmt.Errorf("Error creating stderr pipe %v: %q", container.UUID, stderrErr)
		return
	}

	// Mutex between squeue sync and running sbatch or scancel.
	squeueUpdater.SlurmLock.Lock()
	defer squeueUpdater.SlurmLock.Unlock()

	err := cmd.Start()
	if err != nil {
		submitErr = fmt.Errorf("Error starting %v: %v", cmd.Args, err)
		return
	}

	stdoutChan := make(chan []byte)
	go func() {
		b, _ := ioutil.ReadAll(stdoutReader)
		stdoutReader.Close()
		stdoutChan <- b
	}()

	stderrChan := make(chan []byte)
	go func() {
		b, _ := ioutil.ReadAll(stderrReader)
		stderrReader.Close()
		stderrChan <- b
	}()

	// Send a tiny script on stdin to execute the crunch-run command
	// slurm actually enforces that this must be a #! script
	io.WriteString(stdinWriter, execScript(append(crunchRunCommand, container.UUID)))
	stdinWriter.Close()

	err = cmd.Wait()

	stdoutMsg := <-stdoutChan
	stderrmsg := <-stderrChan

	close(stdoutChan)
	close(stderrChan)

	if err != nil {
		submitErr = fmt.Errorf("Container submission failed: %v: %v (stderr: %q)", cmd.Args, err, stderrmsg)
		return
	}

	log.Printf("sbatch succeeded: %s", strings.TrimSpace(string(stdoutMsg)))
	return
}

// If the container is marked as Locked, check if it is already in the slurm
// queue.  If not, submit it.
//
// If the container is marked as Running, check if it is in the slurm queue.
// If not, mark it as Cancelled.
func monitorSubmitOrCancel(dispatcher *dispatch.Dispatcher, container arvados.Container, monitorDone *bool) {
	submitted := false
	for !*monitorDone {
		if squeueUpdater.CheckSqueue(container.UUID) {
			// Found in the queue, so continue monitoring
			submitted = true
		} else if container.State == dispatch.Locked && !submitted {
			// Not in queue but in Locked state and we haven't
			// submitted it yet, so submit it.

			log.Printf("About to submit queued container %v", container.UUID)

			if err := submit(dispatcher, container, config.CrunchRunCommand); err != nil {
				log.Printf("Error submitting container %s to slurm: %v",
					container.UUID, err)
				// maybe sbatch is broken, put it back to queued
				dispatcher.UpdateState(container.UUID, dispatch.Queued)
			}
			submitted = true
		} else {
			// Not in queue and we are not going to submit it.
			// Refresh the container state. If it is
			// Complete/Cancelled, do nothing, if it is Locked then
			// release it back to the Queue, if it is Running then
			// clean up the record.

			var con arvados.Container
			err := dispatcher.Arv.Get("containers", container.UUID, nil, &con)
			if err != nil {
				log.Printf("Error getting final container state: %v", err)
			}

			var st arvados.ContainerState
			switch con.State {
			case dispatch.Locked:
				st = dispatch.Queued
			case dispatch.Running:
				st = dispatch.Cancelled
			default:
				// Container state is Queued, Complete or Cancelled so stop monitoring it.
				return
			}

			log.Printf("Container %s in state %v but missing from slurm queue, changing to %v.",
				container.UUID, con.State, st)
			dispatcher.UpdateState(container.UUID, st)
		}
	}
}

// Run or monitor a container.
//
// Monitor status updates.  If the priority changes to zero, cancel the
// container using scancel.
func run(dispatcher *dispatch.Dispatcher,
	container arvados.Container,
	status chan arvados.Container) {

	log.Printf("Monitoring container %v started", container.UUID)
	defer log.Printf("Monitoring container %v finished", container.UUID)

	monitorDone := false
	go monitorSubmitOrCancel(dispatcher, container, &monitorDone)

	for container = range status {
		if container.State == dispatch.Locked || container.State == dispatch.Running {
			if container.Priority == 0 {
				log.Printf("Canceling container %s", container.UUID)

				// Mutex between squeue sync and running sbatch or scancel.
				squeueUpdater.SlurmLock.Lock()
				err := scancelCmd(container).Run()
				squeueUpdater.SlurmLock.Unlock()

				if err != nil {
					log.Printf("Error stopping container %s with scancel: %v",
						container.UUID, err)
					if squeueUpdater.CheckSqueue(container.UUID) {
						log.Printf("Container %s is still in squeue after scancel.",
							container.UUID)
						continue
					}
				}

				err = dispatcher.UpdateState(container.UUID, dispatch.Cancelled)
			}
		}
	}
	monitorDone = true
}

func readConfig(dst interface{}, path string) error {
	if buf, err := ioutil.ReadFile(path); err != nil && os.IsNotExist(err) {
		if path == defaultConfigPath {
			log.Printf("Config not specified. Continue with default configuration.")
		} else {
			return fmt.Errorf("Config file not found %q: %v", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("Error reading config %q: %v", path, err)
	} else if err = json.Unmarshal(buf, dst); err != nil {
		return fmt.Errorf("Error decoding config %q: %v", path, err)
	}
	return nil
}
