# k8s-r8
`rotate` is a tool for rotating out AWS ASG managed nodes within a k8s cluster. It was developed to make upgrading AWS AMIs as a one command experience that doesn't require intimate knowledge of AWS commands or Kubernetes internals.

## Assumptions
This tool makes the following assumptions about the setup of your k8s cluster.
1. All nodes in the cluster have a role label
2. Each role label used within the cluster has a 1:1 mapping with an AWS ASG that manages all of the nodes in that role
3. None of the pods running in your cluster maintain persistent volumes (note: this assumption being violated will not cause the tool to fail, but will mean that pods will likely have new volumes on rotation).
4. The name of each node in your cluster is the internal dns name of the node in AWS.

## Requirements
1. An AWS Profile where the default entry has permissions to modify ASGs (for simplicity, permissions to run arbitrary ASG commands is recommend, though this can be locked down as necessary)
2. A Kubernetes config that points to the cluster you wish to rotate. The config's user must have privileges to cordon nodes, delete pods, evict pods, and list the nodes in the cluster.


## Usage

This tool is built via a simple `go build cmd/rotate/main.go` or `go install cmd/rotate`

Ensure none of the target nodes have `SchedulingDisabled` status or the script will block until the node is manually `kubectl uncordon`ed.

### Examples

```
rotate
```
by default the rotate command will find all of the roles in your k8s cluster, and rotate a single node in the ASG corresponding to each role. The default behavior is to rotate the oldest node in the cluster by launch configuration and then by age.

```
rotate --rotate-all
```
This will find all of the roles in your k8s cluster, and incrementally rotate all of the nodes in those ASGs.
ASG need to be the auto scaling group name, eg `safety-staging-ingress-nodes-20181015165147895500000002`. You can find this in AWS console -> ec2 -> auto scaling group
role name needs to be the kubernetes role, eg `ingress` or `compute`. You can find this with `kubectl describe node`.

```
rotate --asg-to-role=ASG:ROLENAME --rotate-all
AWS_PROFILE=twitch-safety-staging AWS_REGION=ap-southeast-3 go run cmd/rotate/main.go --rotate-all --asg-to-role='safety-staging-ingress-nodes-20211015165147895500000001:ingress'
```
This will find the ASG with the given name, make sure it corresponds to the specified role, and if so, rotate all nodes in that ASG incrementally. Removing the `--rotate-all` will lead to the tool rotating a single node.

### Flags
`-f/--strict-delete` - when this flag is enabled, if there are any errors during pod deletion or eviction while spinning down a node, the tool will error out. Otherwise, these errors will be logged and then ignored. The default is that this flag is off.

`-e/--evict-grace-period` - argument - duration - the default value is 5 seconds. This flag controls how long we will wait for a pod to be deleted/evicted before raising an error. This is useful if your pod has a long shutdown protocol.

`-b/--network-backoff` - argument - duration - the default value is 5 seconds. This flag controls the time between requests when the tool is polling the kubernetes cluster to see if a new node has come up or an old node has gone down.

`-r/--asg-to-role` - argument map:ASG-NAME:cluster-role - This takes a mapping of asg names to cluster roles. For each pair, the tool will rotate the oldest node in the cluster. If the `-u/--rotate-all` flag is enabled, then all the nodes in all of the asg/cluster pairs will be rotated. If only one pair is provided, this can be combined with the `-n/--node` flag to specify a list of nodes to rotate in the asg. If no asgs are specified, the tool will search through all ASGs in the target AWS account, and try to correspond them with a role in the cluster. For each mapping that can be determined, the tool will rotate one (or if `-u/--rotate-all` is specified, all) of the nodes those asg/role groups.

`-u/--rotate-all` - Disabled by default. When enabled, this makes it so  that the tool rotates all of the nodes in any asg:role pairings that are targeted.

`-n/--node`- argument, list of k8s node names - This REQUIRES a single ASG:Role pairing be provided via `-r/--asg-to-role`. This flag will make the tool rotate all of the nodes specified in this list.
