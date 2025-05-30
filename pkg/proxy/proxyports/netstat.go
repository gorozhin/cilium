// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package proxyports

import (
	"bytes"
	"os"
	"regexp"
	"strconv"

	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
)

var (
	// procNetFiles is the constant map showing the correspondance of /proc/net files
	// to the bool flag of the "do we expect them to be present based on the config"
	// /proc/net files may be used to get the information about open connections as output by netstat.
	procNetFiles = map[string]bool{
		"/proc/net/tcp":  option.Config.EnableIPv4,
		"/proc/net/udp":  option.Config.EnableIPv4,
		"/proc/net/tcp6": option.Config.EnableIPv6,
		"/proc/net/udp6": option.Config.EnableIPv6,
	}

	// procNetFileRegexp matches the first two columns of /proc/net/{tcp,udp}*
	// files and submatches on the local port number.
	procNetFileRegexp = regexp.MustCompile("^ *[[:digit:]]*: *[[:xdigit:]]*:([[:xdigit:]]*) ")
)

// GetOpenLocalPorts returns the set of L4 ports currently open locally.
func (p *ProxyPorts) GetOpenLocalPorts() map[uint16]struct{} {
	openLocalPorts := make(map[uint16]struct{}, 128)

	for file, enabled := range procNetFiles {
		b, err := os.ReadFile(file)
		if err != nil {
			// we only need to report this as unexpected behaviour
			// when the ipvX is enabled in the config, but not present as a file
			if enabled {
				p.logger.Error("cannot read proc file",
					logfields.Path, file,
					logfields.Error, err,
				)
			}

			continue
		}

		// Extract the local port number from the "local_address" column.
		// The header line won't match and will be ignored.
		for line := range bytes.SplitSeq(b, []byte("\n")) {
			groups := procNetFileRegexp.FindSubmatch(line)
			if len(groups) != 2 { // no match
				continue
			}
			// The port number is in hexadecimal.
			localPort, err := strconv.ParseUint(string(groups[1]), 16, 16)
			if err != nil {
				p.logger.Error("failed to parse port from proc file",
					logfields.Path, file,
					logfields.Error, err,
				)
				continue
			}
			openLocalPorts[uint16(localPort)] = struct{}{}
		}
	}

	return openLocalPorts
}
