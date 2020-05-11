package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/pkg/errors"
	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/clientv3"
	caliconet "github.com/projectcalico/libcalico-go/lib/net"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libcalicoErrors "github.com/projectcalico/libcalico-go/lib/errors"
	wepname "github.com/projectcalico/libcalico-go/lib/names"
	"github.com/projectcalico/libcalico-go/lib/options"
	logutils "github.com/projectcalico/libnetwork-plugin/utils/log"
	mathutils "github.com/projectcalico/libnetwork-plugin/utils/math"
	"github.com/projectcalico/libnetwork-plugin/utils/netns"
	netlink "github.com/vishvananda/netlink"
)

const (
	DOCKER_LABEL_PREFIX       = "org.projectcalico.label."
	LABEL_POLL_TIMEOUT_ENVKEY = "CALICO_LIBNETWORK_LABEL_POLL_TIMEOUT"
	CREATE_PROFILES_ENVKEY    = "CALICO_LIBNETWORK_CREATE_PROFILES"
	LABEL_ENDPOINTS_ENVKEY    = "CALICO_LIBNETWORK_LABEL_ENDPOINTS"
	VETH_MTU_ENVKEY           = "CALICO_LIBNETWORK_VETH_MTU"
	NAMESPACE_ENVKEY          = "CALICO_LIBNETWORK_NAMESPACE"
)

type NetworkDriver struct {
	client         clientv3.Interface
	containerName  string
	orchestratorID string
	namespace      string

	ifPrefix string

	DummyIPV4Nexthop string

	vethMTU uint16

	labelPollTimeout time.Duration

	createProfiles bool
	labelEndpoints bool
}

func NewNetworkDriver(client clientv3.Interface) network.Driver {
	driver := NetworkDriver{
		client: client,

		// Orchestrator and container IDs used in our endpoint identification. These
		// are fixed for libnetwork.  Unique endpoint identification is provided by
		// hostname and endpoint ID.
		containerName:  "libnetwork",
		orchestratorID: "libnetwork",
		namespace:      "libnetwork",

		ifPrefix:         IFPrefix,
		DummyIPV4Nexthop: "169.254.1.1",

		// default: enabled, disable by setting env key to false (case insensitive)
		createProfiles: !strings.EqualFold(os.Getenv(CREATE_PROFILES_ENVKEY), "false"),

		// default: disabled, enable by setting env key to true (case insensitive)
		labelEndpoints: strings.EqualFold(os.Getenv(LABEL_ENDPOINTS_ENVKEY), "true"),
	}

	// Check if MTU environment variable is given, parse into uint16
	// and override the default in the NetworkDriver.
	if mtuStr, ok := os.LookupEnv(VETH_MTU_ENVKEY); ok {
		mtu, err := strconv.ParseUint(mtuStr, 10, 16)
		if err != nil {
			log.Fatalf("Failed to parse %v '%v' into uint16: %v",
				VETH_MTU_ENVKEY, mtuStr, err)
		}

		driver.vethMTU = uint16(mtu)

		log.WithField("mtu", mtu).Info("Parsed veth MTU")
	}

	// Configure namespace
	if value, ok := os.LookupEnv(NAMESPACE_ENVKEY); ok {
		driver.namespace = value

		log.WithField("namespace", value).Info("WorkloadEndpoint namespace changed")
	}

	if !driver.createProfiles {
		log.Info("Feature disabled: no Calico profiles will be created per network")
	}
	if driver.labelEndpoints {
		log.Info("Feature enabled: Calico workloadendpoints will be labelled with Docker labels")
		driver.labelPollTimeout = getLabelPollTimeout()
	}
	return driver
}

func (d NetworkDriver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	resp := network.CapabilitiesResponse{Scope: "global"}
	logutils.JSONMessage("GetCapabilities response", resp)
	return &resp, nil
}

// AllocateNetwork is used for swarm-mode support in remote plugins, which
// Calico's libnetwork-plugin doesn't currently support.
func (d NetworkDriver) AllocateNetwork(request *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	var resp network.AllocateNetworkResponse
	logutils.JSONMessage("AllocateNetwork response", resp)
	return &resp, nil
}

// FreeNetwork is used for swarm-mode support in remote plugins, which
// Calico's libnetwork-plugin doesn't currently support.
func (d NetworkDriver) FreeNetwork(request *network.FreeNetworkRequest) error {
	logutils.JSONMessage("FreeNetwork request", request)
	return nil
}

func (d NetworkDriver) CreateNetwork(request *network.CreateNetworkRequest) error {
	logutils.JSONMessage("CreateNetwork", request)
	opts := map[string]interface{}{
		"com.docker.network.enable_ipv6": true,
		"org.projectcalico.profile":      "",
	}

	err := parseDockerOptions(request.Options, &opts)
	if err != nil {
		return fmt.Errorf("Could not parse options: %s", err.Error())
	}

	ps := []string{}
	for _, ipData := range request.IPv4Data {
		// Older version of Docker have a bug where they don't provide the correct AddressSpace
		// so we can't check for calico IPAM using our known address space.
		// The Docker issue, https://github.com/projectcalico/libnetwork-plugin/issues/77,
		// was fixed sometime between 1.11.2 and 1.12.3.
		// Also the pool might not have a fixed values if --subnet was passed
		// So the only safe thing is to check for our special gateway value
		if ipData.Gateway != "0.0.0.0/0" {
			err := errors.New("Non-Calico IPAM driver is used. Note: Docker before 1.12.3 is unsupported")
			log.Errorln(err)
			return err
		}
		ps = append(ps, ipData.Pool)
	}

	for _, ipData := range request.IPv6Data {
		// Don't support older versions of Docker which have a bug where the correct AddressSpace isn't provided
		if ipData.AddressSpace != CalicoGlobalAddressSpace {
			err := errors.New("Non-Calico IPAM driver is used")
			log.Errorln(err)
			return err
		}
		ps = append(ps, ipData.Pool)
	}

	logutils.JSONMessage("CreateNetwork response", map[string]string{})
	return d.populatePoolAnnotation(ps, request.NetworkID, opts["org.projectcalico.profile"].(string))
}

func (d NetworkDriver) populatePoolAnnotation(pools []string, networkID string, profile string) error {
	ctx := context.Background()
	poolClient := d.client.IPPools()
	ipPools, err := poolClient.List(ctx, options.ListOptions{})
	if err != nil {
		log.Errorln(err)
		return err
	}
	for _, ipPool := range ipPools.Items {
		for _, cidr := range pools {
			if ipPool.Spec.CIDR == cidr {
				ann := ipPool.GetAnnotations()
				if ann == nil {
					ann = map[string]string{}
				}
				ann[DOCKER_LABEL_PREFIX+"network.ID"] = networkID
				if profile != "" {
					ann[DOCKER_LABEL_PREFIX+"profile"] = profile
				}

				ipPool.SetAnnotations(ann)
				_, err = poolClient.Update(ctx, &ipPool, options.SetOptions{})
				if err != nil {
					log.Errorln(err)
					return err
				}
			}
		}
	}
	return nil
}

func (d NetworkDriver) DeleteNetwork(request *network.DeleteNetworkRequest) error {
	logutils.JSONMessage("DeleteNetwork", request)

	ctx := context.Background()
	poolClient := d.client.IPPools()

	pools, err := poolClient.List(ctx, options.ListOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Network %v gather error", request.NetworkID)
		log.Errorln(err)
		return err
	}

	for _, ipPool := range pools.Items {
		if nid, ok := ipPool.Annotations[DOCKER_LABEL_PREFIX+"network.ID"]; ok && nid == request.NetworkID {
			ann := ipPool.GetAnnotations()
			cleanAnn := map[string]string{}

			for k, v := range ann {
				if !strings.HasPrefix(k, DOCKER_LABEL_PREFIX) {
					cleanAnn[k] = v
				}
			}

			ipPool.SetAnnotations(cleanAnn)
			_, err = poolClient.Update(ctx, &ipPool, options.SetOptions{})
			if err != nil {
				log.Errorln(err)
				return err
			}
			break
		}
	}

	return nil
}

func (d NetworkDriver) CreateEndpoint(request *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	logutils.JSONMessage("CreateEndpoint", request)

	ctx := context.Background()

	log.Debugf("Creating endpoint %v\n", request.EndpointID)
	if request.Interface.Address == "" && request.Interface.AddressIPv6 == "" {
		err := errors.New("No address assigned for endpoint")
		log.Errorln(err)
		return nil, err
	}

	var addresses []caliconet.IPNet
	if request.Interface.Address != "" {
		// Parse the address this function was passed. Ignore the subnet - Calico always uses /32 (for IPv4)
		ip4, _, err := net.ParseCIDR(request.Interface.Address)
		log.Debugf("Parsed IP %v from (%v) \n", ip4, request.Interface.Address)

		if err != nil {
			err = errors.Wrapf(err, "Parsing %v as CIDR failed", request.Interface.Address)
			log.Errorln(err)
			return nil, err
		}

		addresses = append(addresses, caliconet.IPNet{IPNet: net.IPNet{IP: ip4, Mask: net.CIDRMask(32, 32)}})
	}

	if request.Interface.AddressIPv6 != "" {
		// Parse the address this function was passed.
		ip6, ipnet, err := net.ParseCIDR(request.Interface.AddressIPv6)
		log.Debugf("Parsed IP %v from (%v) \n", ip6, request.Interface.AddressIPv6)
		if err != nil {
			err = errors.Wrapf(err, "Parsing %v as CIDR failed", request.Interface.AddressIPv6)
			log.Errorln(err)
			return nil, err
		}
		addresses = append(addresses, caliconet.IPNet{IPNet: *ipnet})
	}

	wepName, err := d.generateEndpointName(Hostname, request.EndpointID)
	if err != nil {
		log.Errorln(err)
		return nil, err
	}

	endpoint := api.NewWorkloadEndpoint()
	endpoint.Name = wepName
	endpoint.ObjectMeta.Namespace = d.namespace
	endpoint.Spec.Endpoint = request.EndpointID
	endpoint.Spec.Node = Hostname
	endpoint.Spec.Orchestrator = d.orchestratorID
	endpoint.Spec.Workload = d.containerName
	endpoint.Spec.InterfaceName = "cali" + request.EndpointID[:mathutils.MinInt(11, len(request.EndpointID))]
	var mac net.HardwareAddr
	if request.Interface.MacAddress != "" {
		if mac, err = net.ParseMAC(request.Interface.MacAddress); err != nil {
			err = errors.Wrap(err, "Error parsing MAC address")
			log.Errorln(err)
			return nil, err
		}
	}
	endpoint.Spec.MAC = mac.String()
	for _, addr := range addresses {
		endpoint.Spec.IPNetworks = append(endpoint.Spec.IPNetworks, addr.String())
	}

	pools, err := d.client.IPPools().List(ctx, options.ListOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Network %v gather error", request.NetworkID)
		log.Errorln(err)
		return nil, err
	}

	f := false
	profileName := ""
	for _, p := range pools.Items {
		if nid, ok := p.Annotations[DOCKER_LABEL_PREFIX+"network.ID"]; ok && nid == request.NetworkID {
			f = true
			profileName = p.ObjectMeta.Name
			if profile, ok := p.Annotations[DOCKER_LABEL_PREFIX+"profile"]; ok {
				profileName = profile
			}
			log.Debugf("Find ippool   : %v\n", p.Name)
			log.Debugf("Using profile : %v\n", profileName)
			break
		}
	}
	if !f {
		err := errors.New("The requested subnet must match the CIDR of a configured Calico IP Pool.")
		log.Errorln(err)
		return nil, err
	}

	// Now that we know the network name, set it on the endpoint.
	endpoint.Spec.Profiles = append(endpoint.Spec.Profiles, profileName)

	// Create if missing
	if d.createProfiles {
		if _, err := d.client.Profiles().Get(ctx, profileName, options.GetOptions{}); err != nil {
			// If a profile for the network name doesn't exist then it needs to be created.
			// We always attempt to create the profile and rely on the datastore to reject
			// the request if the profile already exists.
			profile := &api.Profile{
				ObjectMeta: metav1.ObjectMeta{
					Name: profileName,
				},
				Spec: api.ProfileSpec{
					Egress: []api.Rule{{Action: "Allow"}},
					Ingress: []api.Rule{{Action: "Allow",
						Source: api.EntityRule{
							Selector: fmt.Sprintf("has(%s)", profileName),
						}}},
				},
			}
			if _, err := d.client.Profiles().Create(ctx, profile, options.SetOptions{}); err != nil {
				if _, ok := err.(libcalicoErrors.ErrorResourceAlreadyExists); !ok {
					log.Errorln(err)
					return nil, err
				}
			}
		}
	}

	// Create the endpoint last to minimize side-effects if something goes wrong.
	endpoint, err = d.client.WorkloadEndpoints().Create(ctx, endpoint, options.SetOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Workload endpoints creation error, data: %+v", endpoint)
		log.Errorln(err)
		return nil, err
	}

	log.Debugf("Workload created, data: %+v\n", endpoint)

	if d.labelEndpoints {
		go d.populateWorkloadEndpointWithLabels(request, endpoint)
	}

	response := &network.CreateEndpointResponse{Interface: &network.EndpointInterface{}}
	logutils.JSONMessage("CreateEndpoint response", response)
	return response, nil
}

func (d NetworkDriver) DeleteEndpoint(request *network.DeleteEndpointRequest) error {
	logutils.JSONMessage("DeleteEndpoint", request)
	log.Debugf("Removing endpoint %v\n", request.EndpointID)

	wepName, err := d.generateEndpointName(Hostname, request.EndpointID)
	if err != nil {
		log.Errorln(err)
		return err
	}

	if _, err = d.client.WorkloadEndpoints().Delete(
		context.Background(),
		d.namespace,
		wepName,
		options.DeleteOptions{}); err != nil {
		err = errors.Wrapf(err, "Endpoint %v removal error", request.EndpointID)
		log.Errorln(err)
		return err
	}

	logutils.JSONMessage("DeleteEndpoint response JSON={}", map[string]string{})

	return err
}

func (d NetworkDriver) EndpointInfo(request *network.InfoRequest) (*network.InfoResponse, error) {
	logutils.JSONMessage("EndpointInfo", request)
	return nil, nil
}

func (d NetworkDriver) Join(request *network.JoinRequest) (*network.JoinResponse, error) {
	logutils.JSONMessage("Join", request)

	ctx := context.Background()
	// 1) Set up a veth pair
	// 	The one end will stay in the host network namespace - named caliXXXXX
	//	The other end is given a temporary name. It's moved into the final network namespace by libnetwork itself.
	var err error
	prefix := request.EndpointID[:mathutils.MinInt(11, len(request.EndpointID))]
	hostInterfaceName := "cali" + prefix
	tempInterfaceName := "temp" + prefix

	if err = netns.CreateVeth(hostInterfaceName, tempInterfaceName, d.vethMTU); err != nil {
		err = errors.Wrapf(
			err, "Veth creation error, hostInterfaceName=%v, tempInterfaceName=%v, vethMTU=%v",
			hostInterfaceName, tempInterfaceName, d.vethMTU)
		log.Errorln(err)
		return nil, err
	}

	// 2) update workloads
	weps := d.client.WorkloadEndpoints()
	wepName, err := d.generateEndpointName(Hostname, request.EndpointID)
	if err != nil {
		log.Errorln(err)
		return nil, err
	}
	wep, err := weps.Get(ctx, d.namespace, wepName, options.GetOptions{})
	if err != nil {
		log.Errorln(err)
		return nil, err
	}
	tempNIC, err := netlink.LinkByName(tempInterfaceName)
	if err != nil {
		log.Errorln(err)
		return nil, err
	}
	wep.Spec.MAC = tempNIC.Attrs().HardwareAddr.String()
	_, err = weps.Update(ctx, wep, options.SetOptions{})
	if err != nil {
		log.Errorln(err)
		return nil, err
	}

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   tempInterfaceName,
			DstPrefix: IFPrefix,
		},
	}

	// One of the network gateway addresses indicate that we are using
	// Calico IPAM driver.  In this case we setup routes using the gateways
	// configured on the endpoint (which will be our host IPs).
	log.Debugln("Using Calico IPAM driver, configure gateway and static routes to the host")

	resp.Gateway = d.DummyIPV4Nexthop
	resp.StaticRoutes = append(resp.StaticRoutes, &network.StaticRoute{
		Destination: d.DummyIPV4Nexthop + "/32",
		RouteType:   1, // 1 = CONNECTED
		NextHop:     "",
	})

	// NOTE(jkoelker) Check if we should attempt ipv6 config
	ipv6Enabled := true

	os.Setenv("DOCKER_API_VERSION", "1.25")
	dockerCli, err := dockerClient.NewEnvClient()

	if err != nil {
		err = errors.Wrap(
			err,
			"Error while attempting to instantiate docker client from env. "+
				"Disabling IPv6.",
		)
		log.Warningln(err)
		ipv6Enabled = false
	} else {

		defer dockerCli.Close()

		networkData, err := dockerCli.NetworkInspect(ctx, request.NetworkID)
		if err != nil {
			err = errors.Wrapf(
				err,
				"Error inspecting network %s to determine IPv6, forcing disabled",
				request.NetworkID,
			)
			log.Warningln(err)
			ipv6Enabled = false

		} else {
			ipv6Enabled = networkData.EnableIPv6
		}
	}

	linkLocalAddr := netns.GetLinkLocalAddr(hostInterfaceName)
	if linkLocalAddr == nil {
		log.Warnf("No IPv6 link local address for %s", hostInterfaceName)

	} else if !ipv6Enabled {
		log.Warnf("IPv6 disabled for network %s", request.NetworkID)

	} else {
		resp.GatewayIPv6 = fmt.Sprintf("%s", linkLocalAddr)
		nextHopIPv6 := fmt.Sprintf("%s/128", linkLocalAddr)
		resp.StaticRoutes = append(resp.StaticRoutes, &network.StaticRoute{
			Destination: nextHopIPv6,
			RouteType:   1, // 1 = CONNECTED
			NextHop:     "",
		})
	}

	logutils.JSONMessage("Join response", resp)

	return resp, nil
}

func (d NetworkDriver) Leave(request *network.LeaveRequest) error {
	logutils.JSONMessage("Leave response", request)
	caliName := "cali" + request.EndpointID[:mathutils.MinInt(11, len(request.EndpointID))]
	err := netns.RemoveVeth(caliName)
	return err
}

func (d NetworkDriver) DiscoverNew(request *network.DiscoveryNotification) error {
	logutils.JSONMessage("DiscoverNew", request)
	log.Debugln("DiscoverNew response JSON={}")
	return nil
}

func (d NetworkDriver) DiscoverDelete(request *network.DiscoveryNotification) error {
	logutils.JSONMessage("DiscoverDelete", request)
	log.Debugln("DiscoverDelete response JSON={}")
	return nil
}

func (d NetworkDriver) ProgramExternalConnectivity(*network.ProgramExternalConnectivityRequest) error {
	return nil
}

func (d NetworkDriver) RevokeExternalConnectivity(*network.RevokeExternalConnectivityRequest) error {
	return nil
}

// Try to get the container's labels and update the WorkloadEndpoint with them
// Since we do not get container info in the libnetwork API methods we need to
// get them ourselves.
//
// This is how:
// - first we try to get a list of containers attached to the custom network
// - if there is a container with our endpointID, we try to inspect that container
// - any labels for that container prefixed by our 'magic' prefix are added to
//   our WorkloadEndpoint resource
//
// Above may take 1 or more retries, because Docker has to update the
// container list in the NetworkInspect and make the Container available
// for inspecting.
func (d NetworkDriver) populateWorkloadEndpointWithLabels(request *network.CreateEndpointRequest, endpoint *api.WorkloadEndpoint) {
	ctx := context.Background()

	networkID := request.NetworkID
	endpointID := request.EndpointID

	retrySleep := time.Duration(100 * time.Millisecond)

	start := time.Now()
	deadline := start.Add(d.labelPollTimeout)

	os.Setenv("DOCKER_API_VERSION", "1.25")
	dockerCli, err := dockerClient.NewEnvClient()
	if err != nil {
		err = errors.Wrap(err, "Error while attempting to instantiate docker client from env")
		log.Errorln(err)
		return
	}
	defer dockerCli.Close()

RETRY_NETWORK_INSPECT:
	if time.Now().After(deadline) {
		log.Errorf("Getting labels for workloadEndpoint timed out in network inspect loop. Took %s", time.Since(start))
		return
	}

	// inspect our custom network
	networkData, err := dockerCli.NetworkInspect(ctx, networkID)
	if err != nil {
		err = errors.Wrapf(err, "Error inspecting network %s - retrying (T=%s)", networkID, time.Since(start))
		log.Warningln(err)
		// was unable to inspect network, let's retry
		time.Sleep(retrySleep)
		goto RETRY_NETWORK_INSPECT
	}
	logutils.JSONMessage("NetworkInspect response", networkData)

	// try to find the container for which we created an endpoint
	containerID := ""
	for id, containerInNetwork := range networkData.Containers {
		if containerInNetwork.EndpointID == endpointID {
			// skip funky identified containers - observed with dind 1.13.0-rc3, gone in -rc5
			// {
			//   "Containers": {
			//     "ep-736ccfa7cd61ced93b67f7465ddb79633ea6d1f718a8ca7d9d19226f5d3521b0": {
			//       "Name": "run1466946597",
			//       "EndpointID": "736ccfa7cd61ced93b67f7465ddb79633ea6d1f718a8ca7d9d19226f5d3521b0",
			//       ...
			//     }
			//   }
			// }
			if strings.HasPrefix(id, "ep-") {
				log.Debugf("Skipping container entry with matching endpointID, but illegal id: %s", id)
			} else {
				containerID = id
				log.Debugf("Container %s found in NetworkInspect result (T=%s)", containerID, time.Since(start))
				break
			}
		}
	}

	if containerID == "" {
		// cause: Docker has not yet processed the libnetwork CreateEndpoint response.
		log.Warnf("Container not found in NetworkInspect result - retrying (T=%s)", time.Since(start))
		// let's retry
		time.Sleep(retrySleep)
		goto RETRY_NETWORK_INSPECT
	}

RETRY_CONTAINER_INSPECT:
	if time.Now().After(deadline) {
		log.Errorf("Getting labels for workloadEndpoint timed out in container inspect loop. Took %s", time.Since(start))
		return
	}

	containerInfo, err := dockerCli.ContainerInspect(ctx, containerID)
	if err != nil {
		err = errors.Wrapf(err, "Error inspecting container %s for labels - retrying (T=%s)", containerID, time.Since(start))
		log.Warningln(err)
		// was unable to inspect container, let's retry
		time.Sleep(100 * time.Millisecond)
		goto RETRY_CONTAINER_INSPECT
	}

	log.Debugf("Container inspected, processing labels now (T=%s)", time.Since(start))

RETRY_UPDATE_ENDPOINT:
	if time.Now().After(deadline) {
		log.Errorf("Updating endpoint timed out. Took %s", time.Since(start))
		return
	}

	// make sure we have a labels map in the workloadEndpoint
	if endpoint.ObjectMeta.Labels == nil {
		endpoint.ObjectMeta.Labels = map[string]string{}
	}

	labelsFound := 0
	for label, labelValue := range containerInfo.Config.Labels {
		if !strings.HasPrefix(label, DOCKER_LABEL_PREFIX) {
			continue
		}
		labelsFound++
		labelClean := strings.TrimPrefix(label, DOCKER_LABEL_PREFIX)
		endpoint.ObjectMeta.Labels[labelClean] = labelValue
		log.Debugf("Found label for WorkloadEndpoint: %s=%s", labelClean, labelValue)
	}

	if labelsFound == 0 {
		log.Debugf("No labels found for container (T=%s)", time.Since(start))
		return
	}

	// lets update the workloadEndpoint
	_, err = d.client.WorkloadEndpoints().Update(ctx, endpoint, options.SetOptions{})
	if err != nil {
		err = errors.Wrapf(err, "Unable to update WorkloadEndpoint with labels (T=%s)", time.Since(start))
		log.Warningln(err)
		endpoint, err = d.client.WorkloadEndpoints().Get(ctx, endpoint.Namespace, endpoint.Name, options.GetOptions{})
		if err != nil {
			err = errors.Wrapf(err, "Unable to get WorkloadEndpoint (T=%s)", time.Since(start))
			log.Errorln(err)
			return
		}
		time.Sleep(100 * time.Millisecond)
		goto RETRY_UPDATE_ENDPOINT
	}

	log.Infof("WorkloadEndpoint %s updated with labels: %v (T=%s)",
		endpointID, endpoint.ObjectMeta.Labels, time.Since(start))

}

// Returns the label poll timeout. Default is returned unless an environment
// key is set to a valid time.Duration.
func getLabelPollTimeout() time.Duration {
	// 5 seconds should be more than enough for this plugin to get the
	// container labels. More info in func populateWorkloadEndpointWithLabels
	defaultTimeout := time.Duration(5 * time.Second)

	timeoutVal := os.Getenv(LABEL_POLL_TIMEOUT_ENVKEY)
	if timeoutVal == "" {
		return defaultTimeout
	}

	labelPollTimeout, err := time.ParseDuration(timeoutVal)
	if err != nil {
		err = errors.Wrapf(err, "Label poll timeout specified via env key %s is invalid, using default %s",
			LABEL_POLL_TIMEOUT_ENVKEY, defaultTimeout)
		log.Warningln(err)
		return defaultTimeout
	}
	log.Infof("Using custom label poll timeout: %s", labelPollTimeout)
	return labelPollTimeout
}

func (d NetworkDriver) generateEndpointName(hostname, endpointID string) (string, error) {
	wepNameIdent := wepname.WorkloadEndpointIdentifiers{
		Node:         hostname,
		Orchestrator: d.orchestratorID,
		Endpoint:     endpointID,
	}
	return wepNameIdent.CalculateWorkloadEndpointName(false)
}
