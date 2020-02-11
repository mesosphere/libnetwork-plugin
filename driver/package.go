package driver

import (
	"fmt"
	"log"
	"os"

	osutils "github.com/projectcalico/libnetwork-plugin/utils/os"
)

const (
	// Calico IPAM module does not allow selection of pools from which to allocate
	// IP addresses.  The pool ID, which has to be supplied in the libnetwork IPAM
	// API is therefore fixed.  We use different values for IPv4 and IPv6 so that
	// during allocation we know which IP version to use.
	PoolIDV4 = "CalicoPoolIPv4"
	PoolIDV6 = "CalicoPoolIPv6"

	CalicoLocalAddressSpace  = "CalicoLocalAddressSpace"
	CalicoGlobalAddressSpace = "CalicoGlobalAddressSpace"
)

var IFPrefix = "cali"
var Hostname = ""

func init() {
	if value, ok := os.LookupEnv("CALICO_LIBNETWORK_IFPREFIX"); ok {
		IFPrefix = value
		log.Println("Updated CALICO_LIBNETWORK_IFPREFIX to ", value)
	}
	if value, ok := os.LookupEnv("CALICO_LIBNETWORK_HOSTNAME"); ok {
		Hostname = value
		log.Println("Updated CALICO_LIBNETWORK_HOSTNAME to ", value)
	} else {
		hostname, err := osutils.GetHostname()
		if err != nil {
			panic(fmt.Errorf("Unable to obtain hostname: %s", err.Error()))
		} else {
			Hostname = hostname
		}
	}
}
