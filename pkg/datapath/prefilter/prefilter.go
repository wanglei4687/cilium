// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package prefilter

import (
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/cilium/hive/cell"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/datapath/types"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/maps/cidrmap"
	"github.com/cilium/cilium/pkg/option"
)

type preFilterMapType int

const (
	prefixesV4Dyn preFilterMapType = iota
	prefixesV4Fix
	prefixesV6Dyn
	prefixesV6Fix
	mapCount
)

const (
	// Arbitrary chosen for now. We don't preallocate elements,
	// so we could bump the limit if needed later on.
	maxLKeys = 1024 * 64
	maxHKeys = 1024 * 1024 * 20
)

type preFilterMaps [mapCount]*cidrmap.CIDRMap

// PreFilter holds global info on related CIDR maps participating in prefilter
type PreFilter struct {
	logger   *slog.Logger
	maps     preFilterMaps
	revision int64
	mutex    lock.RWMutex

	enabled bool
}

// WriteConfig dumps the configuration for the corresponding header file
func (p *PreFilter) WriteConfig(fw io.Writer) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	fmt.Fprintf(fw, "#define CIDR4_HMAP_ELEMS %d\n", maxHKeys)
	fmt.Fprintf(fw, "#define CIDR4_LMAP_ELEMS %d\n", maxLKeys)

	fmt.Fprintf(fw, "#define CIDR4_FILTER\n")
	fmt.Fprintf(fw, "#define CIDR4_LPM_PREFILTER\n")
	fmt.Fprintf(fw, "#define CIDR6_FILTER\n")
	fmt.Fprintf(fw, "#define CIDR6_LPM_PREFILTER\n")
}

func (p *PreFilter) dumpOneMap(which preFilterMapType, to []string) []string {
	if p.maps[which] == nil {
		return to
	}
	return p.maps[which].CIDRDump(to)
}

func (p *PreFilter) Enabled() bool {
	return p.enabled
}

// Dump dumps revision and CIDRs as string slice of all participating maps
func (p *PreFilter) Dump(to []string) ([]string, int64) {
	if !p.enabled {
		return to, 0
	}

	p.mutex.RLock()
	defer p.mutex.RUnlock()
	for i := prefixesV4Dyn; i < mapCount; i++ {
		to = p.dumpOneMap(i, to)
	}
	return to, p.revision
}

func (p *PreFilter) selectMap(ones, bits int) preFilterMapType {
	if bits == net.IPv4len*8 {
		if ones == bits {
			return prefixesV4Fix
		}
		return prefixesV4Dyn
	} else if bits == net.IPv6len*8 {
		if ones == bits {
			return prefixesV6Fix
		}
		return prefixesV6Dyn
	} else {
		return mapCount
	}
}

// Insert inserts slice of CIDRs (doh!) for the latest revision
func (p *PreFilter) Insert(revision int64, cidrs []net.IPNet) error {
	if !p.enabled {
		return fmt.Errorf("Prefilter is not enabled")
	}

	var undoQueue []net.IPNet
	var ret error

	p.mutex.Lock()
	defer p.mutex.Unlock()
	if revision != 0 && p.revision != revision {
		return fmt.Errorf("Latest revision is %d not %d", p.revision, revision)
	}
	for _, cidr := range cidrs {
		ones, bits := cidr.Mask.Size()
		which := p.selectMap(ones, bits)
		if which == mapCount || p.maps[which] == nil {
			ret = fmt.Errorf("No map enabled for CIDR string %s", cidr.String())
			break
		}
		err := p.maps[which].InsertCIDR(cidr)
		if err != nil {
			ret = fmt.Errorf("Error inserting CIDR string %s: %w", cidr.String(), err)
			break
		} else {
			undoQueue = append(undoQueue, cidr)
		}
	}
	if ret == nil {
		p.revision++
		return ret
	}
	for _, cidr := range undoQueue {
		ones, bits := cidr.Mask.Size()
		which := p.selectMap(ones, bits)
		p.maps[which].DeleteCIDR(cidr)
	}
	return ret
}

// Delete deletes slice of CIDRs (doh!) for the latest revision
func (p *PreFilter) Delete(revision int64, cidrs []net.IPNet) error {
	if !p.enabled {
		return fmt.Errorf("Prefilter is not enabled")
	}

	var undoQueue []net.IPNet
	var ret error

	p.mutex.Lock()
	defer p.mutex.Unlock()
	if revision != 0 && p.revision != revision {
		return fmt.Errorf("Latest revision is %d not %d", p.revision, revision)
	}
	for _, cidr := range cidrs {
		ones, bits := cidr.Mask.Size()
		which := p.selectMap(ones, bits)
		if which == mapCount || p.maps[which] == nil {
			return fmt.Errorf("No map enabled for CIDR string %s", cidr.String())
		}
		// Lets check obvious cases first, so we don't need to painfully unroll
		if !p.maps[which].CIDRExists(cidr) {
			return fmt.Errorf("No map entry for CIDR string %s", cidr.String())
		}
	}
	for _, cidr := range cidrs {
		ones, bits := cidr.Mask.Size()
		which := p.selectMap(ones, bits)
		err := p.maps[which].DeleteCIDR(cidr)
		if err != nil {
			ret = fmt.Errorf("Error deleting CIDR string %s: %w", cidr.String(), err)
			break
		} else {
			undoQueue = append(undoQueue, cidr)
		}
	}
	if ret == nil {
		p.revision++
		return ret
	}
	for _, cidr := range undoQueue {
		ones, bits := cidr.Mask.Size()
		which := p.selectMap(ones, bits)
		p.maps[which].InsertCIDR(cidr)
	}
	return ret
}

func (p *PreFilter) initOneMap(which preFilterMapType) error {
	var prefixdyn bool
	var prefixlen int
	var maxelems uint32
	var path string
	var err error

	switch which {
	case prefixesV4Dyn:
		prefixlen = net.IPv4len * 8
		prefixdyn = true
		maxelems = maxLKeys
		path = bpf.MapPath(p.logger, cidrmap.MapName+"v4_dyn")
	case prefixesV4Fix:
		prefixlen = net.IPv4len * 8
		prefixdyn = false
		maxelems = maxHKeys
		path = bpf.MapPath(p.logger, cidrmap.MapName+"v4_fix")
	case prefixesV6Dyn:
		prefixlen = net.IPv6len * 8
		prefixdyn = true
		maxelems = maxLKeys
		path = bpf.MapPath(p.logger, cidrmap.MapName+"v6_dyn")
	case prefixesV6Fix:
		prefixlen = net.IPv6len * 8
		prefixdyn = false
		maxelems = maxHKeys
		path = bpf.MapPath(p.logger, cidrmap.MapName+"v6_fix")
	}

	p.maps[which], err = cidrmap.OpenMapElems(p.logger, path, prefixlen, prefixdyn, maxelems)
	if err != nil {
		return err
	}
	return nil
}

func (p *PreFilter) init() error {
	for i := prefixesV4Dyn; i < mapCount; i++ {
		if err := p.initOneMap(i); err != nil {
			return err
		}
	}
	return nil
}

// newPreFilter returns prefilter handle
func newPreFilter(logger *slog.Logger, config *option.DaemonConfig, lifecycle cell.Lifecycle) types.PreFilter {
	p := &PreFilter{
		logger:   logger,
		revision: 1,
		enabled:  config.EnableXDPPrefilter,
	}

	if config.EnableXDPPrefilter {
		lifecycle.Append(cell.Hook{
			OnStart: func(hc cell.HookContext) error {
				// Only needed here given we access pinned maps.
				p.mutex.Lock()
				defer p.mutex.Unlock()
				return p.init()
			},
		})
	}

	return p
}
