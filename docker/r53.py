#!/usr/bin/env python

import boto3
import re
import sys
import subprocess

def dig(key, dns_server="8.8.8.8", key_type="a"):
    cmd = ["dig", "+noall", "+answer"]
    cmd += [key, key_type, "@" + dns_server]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE)
    resp = proc.stdout.read()
    proc.stdout.close()
    proc.wait()
    header = ("key", "ttl", "class", "type", "value")
    for record in filter(bool, map(str.strip, resp.split("\n"))):
        record = dict(zip(header, re.split("\s+", record)))
        yield record

def update_record(zone, hostname, ip):
    client = boto3.client("route53")
    resp = client.list_hosted_zones()
    zone_map = {_zone["Name"]: _zone["Id"] for _zone in resp["HostedZones"]}
    zone_id = zone_map.get(zone, zone)
    kw = {
        "HostedZoneId": zone_id,
        "ChangeBatch": {
            "Comment": "I'm a dog!",
            "Changes": [
                {
                    "Action": "CREATE",
                    "ResourceRecordSet": {
                        "Name": "%s.%s" % (hostname, zone),
                        "Type": "A",
                        "TTL": 60,
                        "ResourceRecords": [{"Value": ip}]
                    }
                }
            ]
        }
    }
    print kw
    return client.change_resource_record_sets(**kw)

#resp = update_record("ginkgo.zone.", "test", "10.20.30.40")
#print resp
for rec in dig("www.google.com"):
    print rec
