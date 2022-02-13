package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/aws/aws-sdk-go/aws"

	"code.justin.tv/safety/k8s-rot8/k8s"

	"code.justin.tv/safety/k8s-rot8/rollback"

	awsFuncs "code.justin.tv/safety/k8s-rot8/cloud/aws"

	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/jessevdk/go-flags"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var opts struct {
	FailOnDeleteError bool              `short:"f" long:"strict-delete" description:"do we fail if deleting a pod fails"`
	EvictGracePeriod  time.Duration     `short:"e" long:"evict-grace-period" default:"5s" description:"how long to wait while evicting pods"`
	NetworkBackoff    time.Duration     `short:"b" long:"network-backoff" default:"5s" description:"how long to sleep between checking on cluster status"`
	ASGRoleMapping    map[string]string `short:"r" long:"asg-to-role" description:"mapping between a role in the cluster and an asg to rotate"`
	NodesToRotate     []string          `short:"n" long:"node" description:"a specific node in the cluster to rotate (only valid if only rotating one ASG)"`
	FullRotate        bool              `short:"u"  long:"rotate-all" description:"rotate all nodes in a given asg"`
}

func main() {
	args, err := flags.Parse(&opts)
	if err != nil {
		log.Fatalln("Error parsing commandline arguments: ", err)
	}

	if len(args) != 0 {
		log.Fatalln("Invalid commandline arguments: ", args)
	}

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"),
			"(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal("failed to build kubernetes configuration")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal("failed to create kubernetes clientset")
	}
	sess, err := session.NewSession(aws.NewConfig().WithRegion("us-west-2"))
	if err != nil {
		log.Fatal("failed to create aws session")
	}
	cloud := awsFuncs.New(sess)

	ctx, cancelFunc := context.WithTimeout(context.Background(), 180*time.Minute)
	defer cancelFunc()

	if len(opts.ASGRoleMapping) != 1 && len(opts.NodesToRotate) > 0 {
		log.Fatalln("You can only specify a list of nodes to rotate if are only rotating on asg")
	}

	if len(opts.NodesToRotate) > 0 && opts.FullRotate {
		log.Fatalln("You cannot rotate both a specific subset of nodes and the entire thing")
	}

	if len(opts.ASGRoleMapping) == 0 {

		opts.ASGRoleMapping = make(map[string]string)
		asgPack, err := cloud.DescribeASGsAndInstances(ctx, nil)
		if err != nil {
			log.Fatalln("Failed to list asgs in the account")
		}

		for _, asg := range asgPack {
			if len(asg.Instances) != 0 {
				for _, instance := range asg.Instances {
					name := instance.Name()
					node, err := k8s.FetchNodeByName(clientset, name)
					if err != nil {
						log.Printf("Failed probe for role in asg %s", aws.StringValue(asg.Group.AutoScalingGroupName))
						continue
					}

					foundLabel := false
					for label := range node.Labels {
						if strings.HasPrefix(label, "node-role.kubernetes.io/") {
							role := strings.TrimPrefix(label, "node-role.kubernetes.io/")
							fmt.Printf("role %s\n", label)
							opts.ASGRoleMapping[aws.StringValue(asg.Group.AutoScalingGroupName)] = role
							fmt.Printf("ASG : %s, Role %s\n", aws.StringValue(asg.Group.AutoScalingGroupName), role)
							foundLabel = true
							break
						}
					}
					if foundLabel {
						break
					}
				}

			}
		}
	}

	fmt.Printf("Rotating ASGs!\n")

	filters := []k8s.Filter{k8s.FilterDaemonSet}
	evictOpts := k8s.EvictOptions{
		GracePeriodSeconds: int64(opts.EvictGracePeriod / time.Second),
		FailOnDeleteError:  opts.FailOnDeleteError,
	}
	g, ctx := errgroup.WithContext(ctx)
	for asgName, role := range opts.ASGRoleMapping {
		nodesToRotate := []string{""}
		if len(opts.NodesToRotate) != 0 {
			nodesToRotate = opts.NodesToRotate
		}

		if opts.FullRotate {
			nodesToRotate = nil
			asgPack, err := cloud.DescribeASGsAndInstances(ctx, []string{asgName})
			if err != nil || len(asgPack) != 1 {
				log.Fatalln("Cannot determine the full number of nodes to rotate")
			}
			asg := asgPack[0]
			for _, inst := range asg.Instances {
				nodesToRotate = append(nodesToRotate, inst.Name())
			}

		}

		g.Go(generateScaleOperation(ctx, generateScaleOptInput{
			cloud:         cloud,
			asgName:       asgName,
			role:          role,
			clientset:     clientset,
			evictOpts:     evictOpts,
			filters:       filters,
			nodesToRotate: nodesToRotate,
		}))
	}

	if err = g.Wait(); err != nil {
		log.Printf("Failed to Scale up ASGs, %v\n", err)
	}

}

type generateScaleOptInput struct {
	cloud         *awsFuncs.Clients
	clientset     *kubernetes.Clientset
	evictOpts     k8s.EvictOptions
	filters       []k8s.Filter
	nodesToRotate []string
	asgName       string
	role          string
}

func generateScaleOperation(ctx context.Context, input generateScaleOptInput) func() error {
	return func() error {
		for _, rotateNode := range input.nodesToRotate {
			rollbacker := rollback.New()
			defer func() {
				b, e := rollbacker.Run()
				log.Printf("rollback errors : %v", e)
				if b {
					log.Println("all rollback steps completed")
				}
			}()

			asgPack, err := input.cloud.DescribeASGsAndInstances(ctx, []string{input.asgName})
			if err != nil || len(asgPack) != 1 {
				err = errors.Wrap(err, "failed to describe asgs")
				log.Println(err)
				return err
			}
			asg := asgPack[0]

			asgName := aws.StringValue(asg.Group.AutoScalingGroupName)

			fmt.Printf("Suspending Instance Termination Process in asg %s\n", asgName)
			// suspend termination in the asg
			step, err := input.cloud.SuspendASGTermination(ctx, asg.Group)
			if err != nil {
				log.Println(err)
				return err
			}
			rollbacker.AddStep(step)

			fmt.Printf("Instance Termination Suspended in asg %s\n", asgName)
			fmt.Printf("Scaling up asg %s\n", asgName)
			// scale up the asg
			step, err = input.cloud.ScaleUpASGBy1(ctx, asg.Group)
			if err != nil {
				log.Println(err)
				return err
			}
			rollbacker.AddStep(step)

			fmt.Printf("Sent Scaling Request for asg %s\n", asgName)

			// waiting for scaling to be done
			count, err := input.cloud.WaitingUntilScalingDone(ctx, asg, opts.NetworkBackoff)
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf("Scaling request for asg %s completed \n", asgName)
			fmt.Printf("Waiting for all %d nodes with role %s to appear in cluster\n", count, input.role)
			// waiting for the node to appear in k8s
			err = k8s.WaitForNodes(ctx, input.clientset, input.role, count, opts.NetworkBackoff)
			if err != nil {
				log.Println(err)
				return err
			}
			fmt.Println("All nodes found in the cluster")

			nodeToFind := ""
			if len(input.nodesToRotate) > 0 {
				nodeToFind = rotateNode
			}
			rotateTarget, err := asg.FindRotationTarget(nodeToFind)
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf("Rotating Instance (id: %s, private-dns-name: %s) in ASG %s\n",
				rotateTarget.ID(), rotateTarget.Name(), asgName)

			fmt.Printf("Fetching K8s Info for Instance (id: %s, private-dns-name: %s) in ASG %s\n",
				rotateTarget.ID(), rotateTarget.Name(), asgName)

			node, err := k8s.FetchNodeByName(input.clientset, rotateTarget.Name())
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf("Fetched K8s Info for Instance (id: %s, private-dns-name: %s) in ASG %s\n",
				rotateTarget.ID(), rotateTarget.Name(), asgName)
			fmt.Printf("Cordoning Node %s\n", node.Name)
			// cordon off the node
			step, err = k8s.CordonNode(input.clientset, node)
			if err != nil {
				log.Println(err)
				return err
			}
			rollbacker.AddStep(step)

			fmt.Printf("Cordoned Node %s\n", node.Name)
			fmt.Printf("Determing Pods to Evict from Node %s\n", node.Name)

			pods, err := k8s.PodsToEvict(input.clientset, node.Name, input.evictOpts, input.filters)
			if err != nil {
				log.Println(err)
				return err
			}

			for _, p := range pods {
				fmt.Printf("Evicting pod %s from node %s\n", p.Name, node.Name)
			}
			fmt.Printf("Checking Eviction Support\n")

			policy, err := k8s.CheckEvictionSupport(input.clientset)
			if err != nil {
				log.Println(err)
				return err
			}

			if policy == "" {
				fmt.Println("Eviction not supported, falling back to Deletion")
			} else {
				fmt.Printf("Eviction supported w/ policy version %s\n", policy)
			}

			for _, pod := range pods {
				err := k8s.DeleteOrEvictPod(input.clientset, pod, policy, input.evictOpts)
				if err != nil {
					log.Println(err)
					if input.evictOpts.FailOnDeleteError {
						return err
					}
				}
			}

			// at this point we don't want to undo the work we've done
			rollbacker.Clear()

			fmt.Printf("Terminating Instance (id:%s, name:%s) in asg:%s\n",
				rotateTarget.ID(), rotateTarget.Name(), asgName)
			err = input.cloud.TerminateInstanceInASG(ctx, asg.Group, rotateTarget)
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf("Terminated Instance (id:%s, name:%s) in asg:%s\n",
				rotateTarget.ID(), rotateTarget.Name(), asgName)
			fmt.Printf("Returning Original scale to asg %s\n", asgName)

			err = input.cloud.ReturnToOriginalScale(ctx, asg.Group)
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf("Original scale returned to asg %s\n", asgName)
			fmt.Printf("Resuming Instance Termination Process in asg %s\n", asgName)

			err = input.cloud.ResumeASGTermination(ctx, asg.Group)
			if err != nil {
				log.Println(err)
				return err
			}

			fmt.Printf(" Instance Termination Process Resumed in asg %s\n", asgName)
			rollbacker.Clear()
		}
		return nil
	}
}
