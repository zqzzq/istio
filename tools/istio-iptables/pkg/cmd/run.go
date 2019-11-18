// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cmd

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"istio.io/istio/tools/istio-iptables/pkg/builder"
	"istio.io/istio/tools/istio-iptables/pkg/constants"

	"istio.io/pkg/env"

	"istio.io/istio/tools/istio-iptables/pkg/config"
	dep "istio.io/istio/tools/istio-iptables/pkg/dependencies"
)

type IptablesConfigurator struct {
	iptables *builder.IptablesBuilderImpl
	//TODO(abhide): Fix dep.Dependencies with better interface
	ext dep.Dependencies
	cfg *config.Config
}

func NewIptablesConfigurator(cfg *config.Config) *IptablesConfigurator {
	var ext dep.Dependencies
	if cfg.DryRun {
		ext = &dep.StdoutStubDependencies{}
	} else {
		ext = &dep.RealDependencies{}
	}
	return &IptablesConfigurator{
		iptables: builder.NewIptablesBuilder(),
		ext:      ext,
		cfg:      cfg,
	}
}

type NetworkRange struct {
	IsWildcard bool
	IPNets     []*net.IPNet
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (iptConfigurator *IptablesConfigurator) separateV4V6(cidrList string) (NetworkRange, NetworkRange, error) {
	if cidrList == "*" {
		return NetworkRange{IsWildcard: true}, NetworkRange{IsWildcard: true}, nil
	}
	ipv6Ranges := NetworkRange{IsWildcard: false, IPNets: make([]*net.IPNet, 0)}
	ipv4Ranges := NetworkRange{IsWildcard: false, IPNets: make([]*net.IPNet, 0)}
	for _, ipRange := range split(cidrList) {
		ip, ipNet, err := net.ParseCIDR(ipRange)
		if err != nil {
			_, err = fmt.Fprintf(os.Stderr, "Ignoring error for bug compatibility with istio-iptables.sh: %s\n", err.Error())
			if err != nil {
				return ipv4Ranges, ipv6Ranges, err
			}
			continue
		}
		if ip.To4() != nil {
			ipv4Ranges.IPNets = append(ipv4Ranges.IPNets, ipNet)
		} else {
			ipv6Ranges.IPNets = append(ipv6Ranges.IPNets, ipNet)
		}
	}
	return ipv4Ranges, ipv6Ranges, nil
}

func (iptConfigurator *IptablesConfigurator) logConfig() {
	// Dump out our environment for debugging purposes.
	fmt.Println("Environment:")
	fmt.Println("------------")
	fmt.Println(fmt.Sprintf("ENVOY_PORT=%s", os.Getenv("ENVOY_PORT")))
	fmt.Println(fmt.Sprintf("INBOUND_CAPTURE_PORT=%s", os.Getenv("INBOUND_CAPTURE_PORT")))
	fmt.Println(fmt.Sprintf("ISTIO_INBOUND_INTERCEPTION_MODE=%s", os.Getenv("ISTIO_INBOUND_INTERCEPTION_MODE")))
	fmt.Println(fmt.Sprintf("ISTIO_INBOUND_TPROXY_MARK=%s", os.Getenv("ISTIO_INBOUND_TPROXY_MARK")))
	fmt.Println(fmt.Sprintf("ISTIO_INBOUND_TPROXY_ROUTE_TABLE=%s", os.Getenv("ISTIO_INBOUND_TPROXY_ROUTE_TABLE")))
	fmt.Println(fmt.Sprintf("ISTIO_INBOUND_PORTS=%s", os.Getenv("ISTIO_INBOUND_PORTS")))
	fmt.Println(fmt.Sprintf("ISTIO_LOCAL_EXCLUDE_PORTS=%s", os.Getenv("ISTIO_LOCAL_EXCLUDE_PORTS")))
	fmt.Println(fmt.Sprintf("ISTIO_SERVICE_CIDR=%s", os.Getenv("ISTIO_SERVICE_CIDR")))
	fmt.Println(fmt.Sprintf("ISTIO_SERVICE_EXCLUDE_CIDR=%s", os.Getenv("ISTIO_SERVICE_EXCLUDE_CIDR")))
	fmt.Println("")
	iptConfigurator.cfg.Print()
}

func (iptConfigurator *IptablesConfigurator) handleInboundPortsInclude() {
	// Handling of inbound ports. Traffic will be redirected to Envoy, which will process and forward
	// to the local service. If not set, no inbound port will be intercepted by istio iptablesOrFail.
	var table string
	if iptConfigurator.cfg.InboundPortsInclude != "" {
		if iptConfigurator.cfg.InboundInterceptionMode == constants.TPROXY {
			// When using TPROXY, create a new chain for routing all inbound traffic to
			// Envoy. Any packet entering this chain gets marked with the ${INBOUND_TPROXY_MARK} mark,
			// so that they get routed to the loopback interface in order to get redirected to Envoy.
			// In the ISTIOINBOUND chain, '-j ISTIODIVERT' reroutes to the loopback
			// interface.
			// Mark all inbound packets.
			iptConfigurator.iptables.AppendRuleV4(constants.ISTIODIVERT, constants.MANGLE, "-j", constants.MARK, "--set-mark", iptConfigurator.cfg.InboundTProxyMark)
			iptConfigurator.iptables.AppendRuleV4(constants.ISTIODIVERT, constants.MANGLE, "-j", constants.ACCEPT)
			// Route all packets marked in chain ISTIODIVERT using routing table ${INBOUND_TPROXY_ROUTE_TABLE}.
			//TODO: (abhide): Move this out of this method
			iptConfigurator.ext.RunOrFail(
				dep.IP, "-f", "inet", "rule", "add", "fwmark", iptConfigurator.cfg.InboundTProxyMark, "lookup", iptConfigurator.cfg.InboundTProxyRouteTable)
			// In routing table ${INBOUND_TPROXY_ROUTE_TABLE}, create a single default rule to route all traffic to
			// the loopback interface.
			//TODO: (abhide): Move this out of this method
			err := iptConfigurator.ext.Run(dep.IP, "-f", "inet", "route", "add", "local", "default", "dev", "lo", "table", iptConfigurator.cfg.InboundTProxyRouteTable)
			if err != nil {
				//TODO: (abhide): Move this out of this method
				iptConfigurator.ext.RunOrFail(dep.IP, "route", "show", "table", "all")
			}
			// Create a new chain for redirecting inbound traffic to the common Envoy
			// port.
			// In the ISTIOINBOUND chain, '-j RETURN' bypasses Envoy and
			// '-j ISTIOTPROXY' redirects to Envoy.
			iptConfigurator.iptables.AppendRuleV4(constants.ISTIOTPROXY, constants.MANGLE, "!", "-d", "127.0.0.1/32", "-p", constants.TCP, "-j", constants.TPROXY,
				"--tproxy-mark", iptConfigurator.cfg.InboundTProxyMark+"/0xffffffff", "--on-port", iptConfigurator.cfg.ProxyPort)
			table = constants.MANGLE
		} else {
			table = constants.NAT
		}
		iptConfigurator.iptables.AppendRuleV4(constants.PREROUTING, table, "-p", constants.TCP, "-j", constants.ISTIOINBOUND)

		if iptConfigurator.cfg.InboundPortsInclude == "*" {
			// Makes sure SSH is not redirected
			iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, table, "-p", constants.TCP, "--dport", "22", "-j", constants.RETURN)
			// Apply any user-specified port exclusions.
			if iptConfigurator.cfg.InboundPortsExclude != "" {
				for _, port := range split(iptConfigurator.cfg.InboundPortsExclude) {
					iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, table, "-p", constants.TCP, "--dport", port, "-j", constants.RETURN)
				}
			}
			// Redirect remaining inbound traffic to Envoy.
			if iptConfigurator.cfg.InboundInterceptionMode == constants.TPROXY {
				// If an inbound packet belongs to an established socket, route it to the
				// loopback interface.
				iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, constants.MANGLE, "-p", constants.TCP, "-m", "socket", "-j", constants.ISTIODIVERT)
				// Otherwise, it's a new connection. Redirect it using TPROXY.
				iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, constants.MANGLE, "-p", constants.TCP, "-j", constants.ISTIOTPROXY)
			} else {
				iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, constants.NAT, "-p", constants.TCP, "-j", constants.ISTIOINREDIRECT)
			}
		} else {
			// User has specified a non-empty list of ports to be redirected to Envoy.
			for _, port := range split(iptConfigurator.cfg.InboundPortsInclude) {
				if iptConfigurator.cfg.InboundInterceptionMode == constants.TPROXY {
					iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, constants.MANGLE, "-p", constants.TCP,
						"--dport", port, "-m", "socket", "-j", constants.ISTIODIVERT)
					iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINBOUND, constants.MANGLE, "-p", constants.TCP, "--dport", port, "-m",
						"socket", "-j", constants.ISTIODIVERT)
					iptConfigurator.iptables.AppendRuleV4(
						constants.ISTIOINBOUND, constants.MANGLE, "-p", constants.TCP, "--dport", port, "-j", constants.ISTIOTPROXY)
				} else {
					iptConfigurator.iptables.AppendRuleV4(
						constants.ISTIOINBOUND, constants.NAT, "-p", constants.TCP, "--dport", port, "-j", constants.ISTIOINREDIRECT)
				}
			}
		}
	}
}

func (iptConfigurator *IptablesConfigurator) handleInboundIpv6Rules(ipv6RangesExclude NetworkRange, ipv6RangesInclude NetworkRange) {
	// If ENABLE_INBOUND_IPV6 is unset (default unset), restrict IPv6 traffic.
	if iptConfigurator.cfg.EnableInboundIPv6s != nil {
		var table string
		// Create a new chain for redirecting outbound traffic to the common Envoy port.
		// In both chains, '-j RETURN' bypasses Envoy and '-j ISTIOREDIRECT'
		// redirects to Envoy.
		iptConfigurator.iptables.AppendRuleV6(
			constants.ISTIOREDIRECT, constants.NAT, "-p", constants.TCP, "-j", constants.REDIRECT, "--to-port", iptConfigurator.cfg.ProxyPort)
		// Use this chain also for redirecting inbound traffic to the common Envoy port
		// when not using TPROXY.
		if iptConfigurator.cfg.InboundPortsInclude == "*" {
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINREDIRECT, constants.NAT, "-p", constants.TCP, "-j",
				constants.REDIRECT, "--to-port", iptConfigurator.cfg.InboundCapturePort)
		} else {
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINREDIRECT, constants.NAT, "-p", constants.TCP, "-j",
				constants.REDIRECT, "--to-port", iptConfigurator.cfg.ProxyPort)
		}
		// Handling of inbound ports. Traffic will be redirected to Envoy, which will process and forward
		// to the local service. If not set, no inbound port will be intercepted by istio iptablesOrFail.
		if iptConfigurator.cfg.InboundPortsInclude != "" {
			table = constants.NAT
			iptConfigurator.iptables.AppendRuleV6(constants.PREROUTING, table, "-p", constants.TCP, "-j", constants.ISTIOINBOUND)

			if iptConfigurator.cfg.InboundPortsInclude == "*" {
				// Makes sure SSH is not redirected
				iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINBOUND, table, "-p", constants.TCP, "--dport", "22", "-j", constants.RETURN)
				// Apply any user-specified port exclusions.
				if iptConfigurator.cfg.InboundPortsExclude != "" {
					for _, port := range split(iptConfigurator.cfg.InboundPortsExclude) {
						iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINBOUND, table, "-p", constants.TCP, "--dport", port, "-j", constants.RETURN)
					}
				}
				// Redirect left inbound traffic
				iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINBOUND, table, "-p", constants.TCP, "-j", constants.ISTIOINREDIRECT)
			} else {
				// User has specified a non-empty list of ports to be redirected to Envoy.
				for _, port := range split(iptConfigurator.cfg.InboundPortsInclude) {
					iptConfigurator.iptables.AppendRuleV6(constants.ISTIOINBOUND, constants.NAT, "-p", constants.TCP, "--dport", port, "-j", constants.ISTIOINREDIRECT)
				}
			}
		}
		// Create a new chain for selectively redirecting outbound packets to Envoy.
		// Jump to the ISTIOOUTPUT chain from OUTPUT chain for all tcp traffic.
		iptConfigurator.iptables.AppendRuleV6(constants.OUTPUT, constants.NAT, "-p", constants.TCP, "-j", constants.ISTIOOUTPUT)
		// Apply port based exclusions. Must be applied before connections back to self are redirected.
		if iptConfigurator.cfg.OutboundPortsExclude != "" {
			for _, port := range split(iptConfigurator.cfg.OutboundPortsExclude) {
				iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-p", constants.TCP, "--dport", port, "-j", constants.RETURN)
			}
		}

		// ::6 is bind connect from inbound passthrough cluster
		iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-o", "lo", "-s", "::6/128", "-j", constants.RETURN)

		// Redirect app calls to back itself via Envoy when using the service VIP or endpoint
		// address, e.g. appN => Envoy (client) => Envoy (server) => appN.
		iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-o", "lo", "!", "-d", "::1/128", "-j", constants.ISTIOINREDIRECT)

		for _, uid := range split(iptConfigurator.cfg.ProxyUID) {
			// Avoid infinite loops. Don't redirect Envoy traffic directly back to
			// Envoy for non-loopback traffic.
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-m", "owner", "--uid-owner", uid, "-j", constants.RETURN)
		}

		for _, gid := range split(iptConfigurator.cfg.ProxyGID) {
			// Avoid infinite loops. Don't redirect Envoy traffic directly back to
			// Envoy for non-loopback traffic.
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-m", "owner", "--gid-owner", gid, "-j", constants.RETURN)
		}
		// Skip redirection for Envoy-aware applications and
		// container-to-container traffic both of which explicitly use
		// localhost.
		iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-d", "::1/128", "-j", constants.RETURN)
		// Apply outbound IPv6 exclusions. Must be applied before inclusions.
		for _, cidr := range ipv6RangesExclude.IPNets {
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-d", cidr.String(), "-j", constants.RETURN)
		}
		// Apply outbound IPv6 inclusions.
		if ipv6RangesInclude.IsWildcard {
			// Wildcard specified. Redirect all remaining outbound traffic to Envoy.
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-j", constants.ISTIOREDIRECT)
			for _, internalInterface := range split(iptConfigurator.cfg.KubevirtInterfaces) {
				iptConfigurator.iptables.InsertRuleV6(constants.PREROUTING, constants.NAT, 1, "-i", internalInterface, "-j", constants.RETURN)
			}
		} else if len(ipv6RangesInclude.IPNets) > 0 {
			// User has specified a non-empty list of cidrs to be redirected to Envoy
			for _, cidr := range ipv6RangesInclude.IPNets {
				for _, internalInterface := range split(iptConfigurator.cfg.KubevirtInterfaces) {
					iptConfigurator.iptables.InsertRuleV6(constants.PREROUTING, constants.NAT, 1, "-i", internalInterface,
						"-d", cidr.String(), "-j", constants.ISTIOREDIRECT)
				}
				iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-d", cidr.String(), "-j", constants.ISTIOREDIRECT)
			}
			// All other traffic is not redirected.
			iptConfigurator.iptables.AppendRuleV6(constants.ISTIOOUTPUT, constants.NAT, "-j", constants.RETURN)
		}
	} else {
		// Drop all inbound traffic except established connections.
		iptConfigurator.iptables.AppendRuleV6(constants.INPUT, constants.FILTER, "-m", "state", "--state", "ESTABLISHED", "-j", constants.ACCEPT)
		iptConfigurator.iptables.AppendRuleV6(constants.INPUT, constants.FILTER, "-i", "lo", "-d", "::1", "-j", constants.ACCEPT)
		iptConfigurator.iptables.AppendRuleV6(constants.INPUT, constants.FILTER, "-j", constants.REJECT)
	}
}

func (iptConfigurator *IptablesConfigurator) handleInboundIpv4Rules(ipv4RangesInclude NetworkRange) {
	// Apply outbound IP inclusions.
	if ipv4RangesInclude.IsWildcard {
		// Wildcard specified. Redirect all remaining outbound traffic to Envoy.
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-j", constants.ISTIOREDIRECT)
		for _, internalInterface := range split(iptConfigurator.cfg.KubevirtInterfaces) {
			iptConfigurator.iptables.InsertRuleV4(
				constants.PREROUTING, constants.NAT, 1, "-i", internalInterface, "-j", constants.ISTIOREDIRECT)
		}
	} else if len(ipv4RangesInclude.IPNets) > 0 {
		// User has specified a non-empty list of cidrs to be redirected to Envoy.
		for _, cidr := range ipv4RangesInclude.IPNets {
			for _, internalInterface := range split(iptConfigurator.cfg.KubevirtInterfaces) {
				iptConfigurator.iptables.InsertRuleV4(constants.PREROUTING, constants.NAT, 1, "-i", internalInterface,
					"-d", cidr.String(), "-j", constants.ISTIOREDIRECT)
			}
			iptConfigurator.iptables.AppendRuleV4(
				constants.ISTIOOUTPUT, constants.NAT, "-d", cidr.String(), "-j", constants.ISTIOREDIRECT)
		}
		// All other traffic is not redirected.
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-j", constants.RETURN)
	}
}

func (iptConfigurator *IptablesConfigurator) run() {
	defer func() {
		iptConfigurator.ext.RunOrFail(dep.IPTABLESSAVE)
		iptConfigurator.ext.RunOrFail(dep.IP6TABLESSAVE)
	}()

	// TODO: more flexibility - maybe a whitelist of users to be captured for output instead of a blacklist.
	if iptConfigurator.cfg.ProxyUID == "" {
		usr, err := iptConfigurator.ext.LookupUser()
		var userID string
		// Default to the UID of ENVOY_USER and root
		if err != nil {
			userID = "1337"
		} else {
			userID = usr.Uid
		}
		// If ENVOY_UID is not explicitly defined (as it would be in k8s env), we add root to the list,
		// for ca agent.
		iptConfigurator.cfg.ProxyUID = userID + ",0"
	}

	// for TPROXY as its uid and gid are same
	if iptConfigurator.cfg.ProxyGID == "" {
		iptConfigurator.cfg.ProxyGID = iptConfigurator.cfg.ProxyUID
	}

	podIP, err := iptConfigurator.ext.GetLocalIP()
	if err != nil {
		panic(err)
	}
	// Check if pod's ip is ipv4 or ipv6, in case of ipv6 set variable
	// to program ip6tablesOrFail
	if podIP.To4() == nil {
		iptConfigurator.cfg.EnableInboundIPv6s = podIP
	}

	//
	// Since OUTBOUND_IP_RANGES_EXCLUDE could carry ipv4 and ipv6 ranges
	// need to split them in different arrays one for ipv4 and one for ipv6
	// in order to not to fail
	ipv4RangesExclude, ipv6RangesExclude, err := iptConfigurator.separateV4V6(iptConfigurator.cfg.OutboundIPRangesExclude)
	if err != nil {
		panic(err)
	}
	if ipv4RangesExclude.IsWildcard {
		panic("Invalid value for OUTBOUND_IP_RANGES_EXCLUDE")
	}
	// FixMe: Do we need similar check for ipv6RangesExclude as well ??

	ipv4RangesInclude, ipv6RangesInclude, err := iptConfigurator.separateV4V6(iptConfigurator.cfg.OutboundIPRangesInclude)
	if err != nil {
		panic(err)
	}

	iptConfigurator.logConfig()

	if iptConfigurator.cfg.EnableInboundIPv6s != nil {
		//TODO: (abhide): Move this out of this method
		iptConfigurator.ext.RunOrFail(dep.IP, "-6", "addr", "add", "::6/128", "dev", "lo")
	}

	// Create a new chain for redirecting outbound traffic to the common Envoy port.
	// In both chains, '-j RETURN' bypasses Envoy and '-j ISTIOREDIRECT'
	// redirects to Envoy.
	iptConfigurator.iptables.AppendRuleV4(
		constants.ISTIOREDIRECT, constants.NAT, "-p", constants.TCP, "-j", constants.REDIRECT, "--to-port", iptConfigurator.cfg.ProxyPort)
	// Use this chain also for redirecting inbound traffic to the common Envoy port
	// when not using TPROXY.

	// PROXY_INBOUND_CAPTURE_PORT should be used only user explicitly set INBOUND_PORTS_INCLUDE to capture all
	if iptConfigurator.cfg.InboundPortsInclude == "*" {
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINREDIRECT, constants.NAT, "-p", constants.TCP, "-j", constants.REDIRECT,
			"--to-port", iptConfigurator.cfg.InboundCapturePort)
	} else {
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOINREDIRECT, constants.NAT, "-p", constants.TCP, "-j", constants.REDIRECT,
			"--to-port", iptConfigurator.cfg.ProxyPort)
	}

	iptConfigurator.handleInboundPortsInclude()

	// TODO: change the default behavior to not intercept any output - user may use http_proxy or another
	// iptablesOrFail wrapper (like ufw). Current default is similar with 0.1
	// Jump to the ISTIOOUTPUT chain from OUTPUT chain for all tcp traffic.
	iptConfigurator.iptables.AppendRuleV4(constants.OUTPUT, constants.NAT, "-p", constants.TCP, "-j", constants.ISTIOOUTPUT)
	// Apply port based exclusions. Must be applied before connections back to self are redirected.
	if iptConfigurator.cfg.OutboundPortsExclude != "" {
		for _, port := range split(iptConfigurator.cfg.OutboundPortsExclude) {
			iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-p", constants.TCP, "--dport", port, "-j", constants.RETURN)
		}
	}

	// 127.0.0.6 is bind connect from inbound passthrough cluster
	iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-o", "lo", "-s", "127.0.0.6/32", "-j", constants.RETURN)

	if env.RegisterStringVar("DISABLE_REDIRECTION_ON_LOCAL_LOOPBACK", "", "").Get() == "" {
		// Redirect app calls back to itself via Envoy when using the service VIP or endpoint
		// address, e.g. appN => Envoy (client) => Envoy (server) => appN.
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-o", "lo", "!", "-d", "127.0.0.1/32", "-j", constants.ISTIOINREDIRECT)
	}

	for _, uid := range split(iptConfigurator.cfg.ProxyUID) {
		// Avoid infinite loops. Don't redirect Envoy traffic directly back to
		// Envoy for non-loopback traffic.
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-m", "owner", "--uid-owner", uid, "-j", constants.RETURN)
	}

	for _, gid := range split(iptConfigurator.cfg.ProxyGID) {
		// Avoid infinite loops. Don't redirect Envoy traffic directly back to
		// Envoy for non-loopback traffic.
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-m", "owner", "--gid-owner", gid, "-j", constants.RETURN)
	}
	// Skip redirection for Envoy-aware applications and
	// container-to-container traffic both of which explicitly use
	// localhost.
	iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-d", "127.0.0.1/32", "-j", constants.RETURN)
	// Apply outbound IPv4 exclusions. Must be applied before inclusions.
	for _, cidr := range ipv4RangesExclude.IPNets {
		iptConfigurator.iptables.AppendRuleV4(constants.ISTIOOUTPUT, constants.NAT, "-d", cidr.String(), "-j", constants.RETURN)
	}

	for _, internalInterface := range split(iptConfigurator.cfg.KubevirtInterfaces) {
		iptConfigurator.iptables.InsertRuleV4(constants.PREROUTING, constants.NAT, 1, "-i", internalInterface, "-j", constants.RETURN)
	}

	iptConfigurator.handleInboundIpv4Rules(ipv4RangesInclude)
	iptConfigurator.handleInboundIpv6Rules(ipv6RangesExclude, ipv6RangesInclude)

	iptConfigurator.executeCommands()
}

func (iptConfigurator *IptablesConfigurator) createRulesFile(f *os.File, contents string) error {
	defer f.Close()
	writer := bufio.NewWriter(f)
	_, err := writer.WriteString(contents)
	if err != nil {
		return fmt.Errorf("Unable to write iptables-restore file: %v", err)
	}
	err = writer.Flush()
	return err
}

func (iptConfigurator *IptablesConfigurator) executeIptablesCommands(commands [][]string) {
	for _, cmd := range commands {
		if len(cmd) > 1 {
			iptConfigurator.ext.RunOrFail(cmd[0], cmd[1:]...)
		} else {
			iptConfigurator.ext.RunOrFail(cmd[0])
		}
	}
}

func (iptConfigurator *IptablesConfigurator) executeIptablesRestoreCommand(isIpv4 bool) error {
	var data, filename, cmd string
	if isIpv4 {
		data = iptConfigurator.iptables.BuildV4Restore()
		filename = fmt.Sprintf("iptables-rules-%d.txt", time.Now().UnixNano())
		cmd = constants.IPTABLESRESTORE
	} else {
		data = iptConfigurator.iptables.BuildV6Restore()
		filename = fmt.Sprintf("ip6tables-rules-%d.txt", time.Now().UnixNano())
		cmd = constants.IP6TABLESRESTORE
	}
	rulesFile, err := ioutil.TempFile("", filename)
	defer os.Remove(rulesFile.Name())
	if err != nil {
		return fmt.Errorf("unable to create iptables-restore file: %v", err)
	}
	if err := iptConfigurator.createRulesFile(rulesFile, data); err != nil {
		return err
	}
	// --noflush to prevent flushing/deleting previous contents from table
	iptConfigurator.ext.RunOrFail(cmd, "--noflush", rulesFile.Name())
	return nil
}

func (iptConfigurator *IptablesConfigurator) executeCommands() {
	if iptConfigurator.cfg.RestoreFormat {
		// Execute iptables-restore
		err := iptConfigurator.executeIptablesRestoreCommand(true)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		// Execute ip6tables-restore
		err = iptConfigurator.executeIptablesRestoreCommand(false)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	} else {
		// Execute iptables commands
		iptConfigurator.executeIptablesCommands(iptConfigurator.iptables.BuildV4())
		// Execute ip6tables commands
		iptConfigurator.executeIptablesCommands(iptConfigurator.iptables.BuildV6())

	}
}
