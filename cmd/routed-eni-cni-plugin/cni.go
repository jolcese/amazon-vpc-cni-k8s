// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// AWS VPC CNI plugin binary
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	cniSpecVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/aws/amazon-vpc-cni-k8s/cmd/routed-eni-cni-plugin/driver"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/grpcwrapper"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/ipamd/datastore"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/networkutils"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/rpcwrapper"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/typeswrapper"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/utils/logger"
	pb "github.com/aws/amazon-vpc-cni-k8s/rpc"
)

const ipamdAddress = "127.0.0.1:50051"

const vlanInterfacePrefix = "vlan"
const dummyVlanInterfacePrefix = "dummy"

var version string

// NetConf stores the common network config for the CNI plugin
type NetConf struct {
	types.NetConf

	// VethPrefix is the prefix to use when constructing the host-side
	// veth device name. It should be no more than four characters, and
	// defaults to 'eni'.
	VethPrefix string `json:"vethPrefix"`

	// MTU for eth0
	MTU string `json:"mtu"`

	PluginLogFile string `json:"pluginLogFile"`

	PluginLogLevel string `json:"pluginLogLevel"`
}

// K8sArgs is the valid CNI_ARGS used for Kubernetes
type K8sArgs struct {
	types.CommonArgs

	// K8S_POD_NAME is pod's name
	K8S_POD_NAME types.UnmarshallableString

	// K8S_POD_NAMESPACE is pod's namespace
	K8S_POD_NAMESPACE types.UnmarshallableString

	// K8S_POD_INFRA_CONTAINER_ID is pod's sandbox id
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

func init() {
	// This is to ensure that all the namespace operations are performed for
	// a single thread
	runtime.LockOSThread()
}

// LoadNetConf converts inputs (i.e. stdin) to NetConf
func LoadNetConf(bytes []byte) (*NetConf, logger.Logger, error) {
	// Default config
	conf := NetConf{
		MTU:        "9001",
		VethPrefix: "eni",
	}

	if err := json.Unmarshal(bytes, &conf); err != nil {
		return nil, nil, errors.Wrap(err, "add cmd: error loading config from args")
	}

	if conf.RawPrevResult != nil {
		if err := cniSpecVersion.ParsePrevResult(&conf.NetConf); err != nil {
			return nil, nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
	}

	logConfig := logger.Configuration{
		LogLevel:    conf.PluginLogLevel,
		LogLocation: conf.PluginLogFile,
	}
	log := logger.New(&logConfig)

	if len(conf.VethPrefix) > 4 {
		return nil, nil, errors.New("conf.VethPrefix can be at most 4 characters long")
	}
	return &conf, log, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	return add(args, typeswrapper.New(), grpcwrapper.New(), rpcwrapper.New(), driver.New())
}

func add(args *skel.CmdArgs, cniTypes typeswrapper.CNITYPES, grpcClient grpcwrapper.GRPC,
	rpcClient rpcwrapper.RPC, driverClient driver.NetworkAPIs) error {

	conf, log, err := LoadNetConf(args.StdinData)
	if err != nil {
		return errors.Wrap(err, "add cmd: error loading config from args")
	}

	log.Infof("Received CNI add request: ContainerID(%s) Netns(%s) IfName(%s) Args(%s) Path(%s) argsStdinData(%s)",
		args.ContainerID, args.Netns, args.IfName, args.Args, args.Path, args.StdinData)

	log.Debugf("Prev Result: %v\n", conf.PrevResult)

	var k8sArgs K8sArgs
	if err := cniTypes.LoadArgs(args.Args, &k8sArgs); err != nil {
		log.Errorf("Failed to load k8s config from arg: %v", err)
		return errors.Wrap(err, "add cmd: failed to load k8s config from arg")
	}

	mtu := networkutils.GetEthernetMTU(conf.MTU)
	log.Debugf("MTU value set is %d:", mtu)

	// Set up a connection to the ipamD server.
	conn, err := grpcClient.Dial(ipamdAddress, grpc.WithInsecure())
	if err != nil {
		log.Errorf("Failed to connect to backend server for container %s: %v",
			args.ContainerID, err)
		return errors.Wrap(err, "add cmd: failed to connect to backend server")
	}
	defer conn.Close()

	c := rpcClient.NewCNIBackendClient(conn)

	r, err := c.AddNetwork(context.Background(),
		&pb.AddNetworkRequest{
			ClientVersion:              version,
			K8S_POD_NAME:               string(k8sArgs.K8S_POD_NAME),
			K8S_POD_NAMESPACE:          string(k8sArgs.K8S_POD_NAMESPACE),
			K8S_POD_INFRA_CONTAINER_ID: string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID),
			Netns:                      args.Netns,
			ContainerID:                args.ContainerID,
			NetworkName:                conf.Name,
			IfName:                     args.IfName,
		})

	if err != nil {
		log.Errorf("Error received from AddNetwork grpc call for containerID %s: %v",
			args.ContainerID,
			err)
		return errors.Wrap(err, "add cmd: Error received from AddNetwork gRPC call")
	}

	if !r.Success {
		log.Errorf("Failed to assign an IP address to container %s",
			args.ContainerID)
		return errors.New("add cmd: failed to assign an IP address to container")
	}

	log.Infof("Received add network response for container %s interface %s: %+v",
		args.ContainerID, args.IfName, r)

	//We will let the values in result struct guide us in terms of IP Address Family configured.
	var v4Addr, v6Addr, addr *net.IPNet
	var addrFamily string

	//We don't support dual stack mode currently so it has to be either v4 or v6 mode.
	if r.IPv4Addr != "" {
		v4Addr = &net.IPNet{
			IP:   net.ParseIP(r.IPv4Addr),
			Mask: net.CIDRMask(32, 32),
		}
		addrFamily = "4"
		addr = v4Addr
	} else if r.IPv6Addr != "" {
		v6Addr = &net.IPNet{
			IP:   net.ParseIP(r.IPv6Addr),
			Mask: net.CIDRMask(128, 128),
		}
		addrFamily = "6"
		addr = v6Addr
	}

	var hostVethName string
	var dummyVlanInterface *current.Interface

	// Non-zero value means pods are using branch ENI
	if r.PodVlanId != 0 {
		hostVethName = generateHostVethName(vlanInterfacePrefix, string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_NAME))
		err = driverClient.SetupPodENINetwork(hostVethName, args.IfName, args.Netns, v4Addr, v6Addr, int(r.PodVlanId), r.PodENIMAC,
			r.PodENISubnetGW, int(r.ParentIfIndex), mtu, log)

		// This is a dummyVlanInterfaceName generated to identify dummyVlanInterface
		// which will be created for PPSG scenario to pass along the vlanId information
		// as a part of the ADD cmd Result struct
		// The podVlanId is used by DEL cmd, fetched from the prevResult struct to cleanup the pod network
		dummyVlanInterfaceName := generateHostVethName(dummyVlanInterfacePrefix, string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_NAME))

		// The dummyVlanInterface is purely virtual and relevent only for ppsg, so we decided to keep it separate
		// and not overload the already available hostVethInterface
		dummyVlanInterface = &current.Interface{Name: dummyVlanInterfaceName, Mac: fmt.Sprint(r.PodVlanId)}
		log.Debugf("Using dummy vlanInterface: %v", dummyVlanInterface)
	} else {
		// build hostVethName
		// Note: the maximum length for linux interface name is 15
		hostVethName = generateHostVethName(conf.VethPrefix, string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_NAME))
		err = driverClient.SetupNS(hostVethName, args.IfName, args.Netns, v4Addr, v6Addr, int(r.DeviceNumber), r.VPCv4CIDRs, r.UseExternalSNAT, mtu, log)
	}

	if err != nil {
		log.Errorf("Failed SetupPodNetwork for container %s: %v",
			args.ContainerID, err)

		// return allocated IP back to IP pool
		r, delErr := c.DelNetwork(context.Background(), &pb.DelNetworkRequest{
			ClientVersion:              version,
			K8S_POD_NAME:               string(k8sArgs.K8S_POD_NAME),
			K8S_POD_NAMESPACE:          string(k8sArgs.K8S_POD_NAMESPACE),
			K8S_POD_INFRA_CONTAINER_ID: string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID),
			ContainerID:                args.ContainerID,
			IfName:                     args.IfName,
			NetworkName:                conf.Name,
			Reason:                     "SetupNSFailed",
		})

		if delErr != nil {
			log.Errorf("Error received from DelNetwork grpc call for container %s: %v",
				args.ContainerID, delErr)
		} else if !r.Success {
			log.Errorf("Failed to release IP of container %s", args.ContainerID)
		}
		return errors.Wrap(err, "add command: failed to setup network")
	}

	containerInterfaceIndex := 1
	ips := []*current.IPConfig{
		{
			Version:   addrFamily,
			Address:   *addr,
			Interface: &containerInterfaceIndex,
		},
	}

	hostInterface := &current.Interface{Name: hostVethName}
	containerInterface := &current.Interface{Name: args.IfName, Sandbox: args.Netns}

	result := &current.Result{
		IPs: ips,
		Interfaces: []*current.Interface{
			hostInterface,
			containerInterface,
		},
	}

	// We append dummyVlanInterface only for pods using branch ENI
	if dummyVlanInterface != nil {
		result.Interfaces = append(result.Interfaces, dummyVlanInterface)
	}

	return cniTypes.PrintResult(result, conf.CNIVersion)
}

// generateHostVethName returns a name to be used on the host-side veth device.
// The veth name is generated such that it aligns with the value expected
// by Calico for NetworkPolicy enforcement.
func generateHostVethName(prefix, namespace, podname string) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s.%s", namespace, podname)))
	return fmt.Sprintf("%s%s", prefix, hex.EncodeToString(h.Sum(nil))[:11])
}

func cmdDel(args *skel.CmdArgs) error {
	return del(args, typeswrapper.New(), grpcwrapper.New(), rpcwrapper.New(), driver.New())
}

func del(args *skel.CmdArgs, cniTypes typeswrapper.CNITYPES, grpcClient grpcwrapper.GRPC, rpcClient rpcwrapper.RPC,
	driverClient driver.NetworkAPIs) error {

	conf, log, err := LoadNetConf(args.StdinData)
	log.Debugf("Prev Result: %v\n", conf.PrevResult)

	if err != nil {
		return errors.Wrap(err, "add cmd: error loading config from args")
	}

	log.Infof("Received CNI del request: ContainerID(%s) Netns(%s) IfName(%s) Args(%s) Path(%s) argsStdinData(%s)",
		args.ContainerID, args.Netns, args.IfName, args.Args, args.Path, args.StdinData)

	var k8sArgs K8sArgs
	if err := cniTypes.LoadArgs(args.Args, &k8sArgs); err != nil {
		log.Errorf("Failed to load k8s config from args: %v", err)
		return errors.Wrap(err, "del cmd: failed to load k8s config from args")
	}

	// With containerd as the runtime, it was observed that sometimes spurious delete requests
	// are triggered from kubelet with an empty Netns. This check safeguards against such
	// scenarios and we just return
	// ref: https://github.com/kubernetes/kubernetes/issues/44100#issuecomment-329780382
	if args.Netns == "" {
		log.Info("Netns() is empty, so network already cleanedup. Nothing to do")
		return nil
	}
	prevResult, ok := conf.PrevResult.(*current.Result)

	// Try to use prevResult if available
	// prevResult might not be availabe, if we are still using older cni spec < 0.4.0.
	// So we should fallback to the old clean up method
	if ok {
		dummyVlanInterfaceName := generateHostVethName(dummyVlanInterfacePrefix, string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_NAME))
		for _, iface := range prevResult.Interfaces {
			if iface.Name == dummyVlanInterfaceName {
				podVlanId, err := strconv.Atoi(iface.Mac)
				if err != nil {
					log.Errorf("Failed to parse vlanId from prevResult: %v", err)
					return errors.Wrap(err, "del cmd: failed to parse vlanId from prevResult")
				}

				// podVlanID can not be 0 as we add dummyVlanInterface only for ppsg
				// if it is 0 then we should return an error
				if podVlanId == 0 {
					log.Errorf("Found SG pod:%s namespace:%s with 0 vlanID", k8sArgs.K8S_POD_NAME, k8sArgs.K8S_POD_NAMESPACE)
					return errors.Wrap(err, "del cmd: found Incorrect 0 vlandId for ppsg")
				}

				err = cleanUpPodENI(podVlanId, log, args.ContainerID, driverClient)
				if err != nil {
					return err
				}
				log.Infof("Received del network response for pod %s namespace %s sandbox %s with vlanId: %v", string(k8sArgs.K8S_POD_NAME),
					string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID), podVlanId)
				return nil
			}
		}
	}

	// notify local IP address manager to free secondary IP
	// Set up a connection to the server.
	conn, err := grpcClient.Dial(ipamdAddress, grpc.WithInsecure())
	if err != nil {
		log.Errorf("Failed to connect to backend server for container %s: %v",
			args.ContainerID, err)

		return errors.Wrap(err, "del cmd: failed to connect to backend server")
	}
	defer conn.Close()

	c := rpcClient.NewCNIBackendClient(conn)

	r, err := c.DelNetwork(context.Background(), &pb.DelNetworkRequest{
		ClientVersion:              version,
		K8S_POD_NAME:               string(k8sArgs.K8S_POD_NAME),
		K8S_POD_NAMESPACE:          string(k8sArgs.K8S_POD_NAMESPACE),
		K8S_POD_INFRA_CONTAINER_ID: string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID),
		NetworkName:                conf.Name,
		ContainerID:                args.ContainerID,
		IfName:                     args.IfName,
		Reason:                     "PodDeleted",
	})

	if err != nil {
		if strings.Contains(err.Error(), datastore.ErrUnknownPod.Error()) {
			// Plugins should generally complete a DEL action without error even if some resources are missing. For example,
			// an IPAM plugin should generally release an IP allocation and return success even if the container network
			// namespace no longer exists, unless that network namespace is critical for IPAM management
			log.Infof("Container %s not found", args.ContainerID)
			return nil
		}
		log.Errorf("Error received from DelNetwork gRPC call for container %s: %v",
			args.ContainerID, err)
		return errors.Wrap(err, "del cmd: error received from DelNetwork gRPC call")
	}

	if !r.Success {
		log.Errorf("Failed to process delete request for container %s: Success == false",
			args.ContainerID)
		return errors.New("del cmd: failed to process delete request")
	}

	log.Infof("Received del network response for pod %s namespace %s sandbox %s: %+v", string(k8sArgs.K8S_POD_NAME),
		string(k8sArgs.K8S_POD_NAMESPACE), string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID), r)

	var deletedPodIP net.IP
	var maskLen int
	if r.IPv4Addr != "" {
		deletedPodIP = net.ParseIP(r.IPv4Addr)
		maskLen = 32
	} else if r.IPv6Addr != "" {
		deletedPodIP = net.ParseIP(r.IPv6Addr)
		maskLen = 128
	}

	if deletedPodIP != nil {
		addr := &net.IPNet{
			IP:   deletedPodIP,
			Mask: net.CIDRMask(maskLen, maskLen),
		}

		if r.PodVlanId != 0 {
			err = driverClient.TeardownPodENINetwork(int(r.PodVlanId), log)
		} else {
			err = driverClient.TeardownNS(addr, int(r.DeviceNumber), log)
		}

		if err != nil {
			log.Errorf("Failed on TeardownPodNetwork for container ID %s: %v",
				args.ContainerID, err)
			return errors.Wrap(err, "del cmd: failed on tear down pod network")
		}
	} else {
		log.Warnf("Container %s did not have a valid IP %s", args.ContainerID, r.IPv4Addr)
	}
	return nil
}

func cleanUpPodENI(podVlanId int, log logger.Logger, containerId string, driverClient driver.NetworkAPIs) error {
	err := driverClient.TeardownPodENINetwork(podVlanId, log)
	if err != nil {
		log.Errorf("Failed on TeardownPodNetwork for container ID %s: %v",
			containerId, err)
		return errors.Wrap(err, "del cmd: failed on tear down pod network")
	}
	return nil
}

func main() {
	log := logger.DefaultLogger()
	about := fmt.Sprintf("AWS CNI %s", version)
	exitCode := 0
	if e := skel.PluginMainWithError(cmdAdd, nil, cmdDel, cniSpecVersion.All, about); e != nil {
		if err := e.Print(); err != nil {
			log.Errorf("Failed to write error to stdout: %v", err)
		}
		exitCode = 1
	}
	os.Exit(exitCode)
}
