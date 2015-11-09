package main

import (
	"aghajoon/dhcp"
	"aghajoon/logging"
	"aghajoon/pxe"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"
)

//go:generate go-bindata -o pxe/pxelinux_autogen.go -prefix=pxelinux -ignore=README.md pxe/pxelinux

const (
	workspacePathHelp = `Path to workspace which obey following structure
		/images/{core-os-version}/coreos_production_pxe_image.cpio.gz
		/images/{core-os-version}/coreos_production_pxe.vmlinuz
		/cloudconfig/
		/ignitionconfig/
`
)

var (
	debug             *bool
	listenIFFlag      *string = flag.String("if", "0.0.0.0", "Interface name for DHCP and PXE to listen on")
	workspacePathFlag *string = flag.String("workspace", "/workspace", workspacePathHelp)
	etcdFlag          *string = flag.String("etcd", "", "Etcd endpoints")
	etcdDirFlag       *string = flag.String("etcd-dir", "aghajoon", "The etcd directory used by this instance of aghajoon")

	leaseStartFlag  *string = flag.String("lease-start", "", "Begining of lease starting IP")
	leaseRangeFlag  *int    = flag.Int("lease-range", 0, "Lease range")
	leaseSubnetFlag *string = flag.String("lease-subnet", "", "Subnet of specified lease")
	leaseRouterFlag *string = flag.String("router", "", "Default router that assigned to DHCP clients")
	leaseDNSFlag    *string = flag.String("dns", "", "Default DNS that assigned to DHCP clients")
)

func interfaceIP(iface *net.Interface) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	fs := [](func(net.IP) bool){
		net.IP.IsGlobalUnicast,
		net.IP.IsLinkLocalUnicast,
		net.IP.IsLoopback,
	}
	for _, f := range fs {
		for _, a := range addrs {
			ipaddr, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipaddr.IP.To4()
			if ip == nil {
				continue
			}
			if f(ip) {
				return ip, nil
			}
		}
	}
	return nil, fmt.Errorf("interface %s has no usable unicast addresses", iface.Name)
}

func main() {
	flag.Parse()
	// etcd config
	if etcdFlag == nil || etcdDirFlag == nil {
		fmt.Fprint(os.Stderr, "please specify the etcd endpoints\n")
		os.Exit(1)
	}
	var err error

	// listen ip address for http, tftp
	var listenIP net.IP = net.IP{0, 0, 0, 0}
	// finding interface by interface name
	var dhcpIF *net.Interface
	if *listenIFFlag != "" {
		dhcpIF, err = net.InterfaceByName(*listenIFFlag)
	} else {
		fmt.Fprint(os.Stderr, "please specify an interface\n")
		os.Exit(1)
	}
	if err != nil {
		log.Fatalln(err)
	}

	dhcpIP, err := interfaceIP(dhcpIF)
	if err != nil {
		log.Fatalln(err)
	}

	// used for replying in dhcp and pxe
	var serverIP net.IP = net.IPv4zero
	if serverIP.Equal(net.IPv4zero) {
		serverIP = dhcpIP
	}

	var httpAddr = net.TCPAddr{IP: listenIP, Port: 70}
	var tftpAddr = net.UDPAddr{IP: listenIP, Port: 69}
	var pxeAddr = net.UDPAddr{IP: dhcpIP, Port: 4011}

	// dhcp setting
	leaseStart := net.ParseIP(*leaseStartFlag)
	leaseRange := *leaseRangeFlag
	leaseSubnet := net.ParseIP(*leaseSubnetFlag)
	leaseRouter := net.ParseIP(*leaseRouterFlag)
	leaseDNS := net.ParseIP(*leaseDNSFlag)
	leaseDuration := 1 * time.Hour

	if leaseStart == nil {
		fmt.Fprint(os.Stderr, "please specify the lease start ip\n")
		os.Exit(1)
	}
	if leaseRange <= 1 {
		fmt.Fprint(os.Stderr, "lease range should be greater that 1\n")
		os.Exit(1)
	}
	if leaseSubnet == nil {
		fmt.Fprint(os.Stderr, "please specify the lease subnet\n")
		os.Exit(1)
	}
	if leaseRouter == nil {
		fmt.Fprint(os.Stderr, "please specify the IP address of network router\n")
		os.Exit(1)
	}
	if leaseDNS == nil {
		fmt.Fprint(os.Stderr, "please specify an DNS server\n")
		os.Exit(1)
	}

	// Printing stat
	fmt.Printf("Server IP:		%s\n	", serverIP.String())
	fmt.Printf("Interface IP:	%s\n", dhcpIP.String())
	fmt.Printf("Interface Name:	%s\n", dhcpIF.Name)

	// serving http
	go func() {
		log.Fatalln(pxe.ServeHTTPBooter(httpAddr, *workspacePathFlag))
	}()
	// serving tftp
	go func() {
		log.Fatalln(pxe.ServeTFTP(tftpAddr))
	}()
	// pxe protocol
	go func() {
		log.Fatalln(pxe.ServePXE(pxeAddr, serverIP, net.TCPAddr{IP: serverIP, Port: httpAddr.Port}))
	}()
	// serving dhcp
	leasePool, err := dhcp.NewLeasePool(*etcdFlag, *etcdDirFlag, leaseStart, leaseRange, leaseDuration)
	if err != nil {
		log.Fatalln(err)
	}
	go func() {
		log.Fatalln(dhcp.ServeDHCP(&dhcp.DHCPSetting{
			IFName:        dhcpIF.Name,
			LeaseDuration: leaseDuration,
			ServerIP:      serverIP,
			RouterAddr:    leaseRouter,
			SubnetMask:    leaseSubnet,
			DNSAddr:       leaseDNS,
		}, leasePool))
	}()
	logging.RecordLogs(true)
}
