package astilibav

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/asticode/go-astiencoder"
	"github.com/asticode/go-astikit"
	"github.com/asticode/goav/avformat"
)

var countMuxer uint64

// Muxer represents an object capable of muxing packets into an output
type Muxer struct {
	*astiencoder.BaseNode
	c                *astikit.Chan
	cl               *astikit.Closer
	ctxFormat        *avformat.Context
	eh               *astiencoder.EventHandler
	o                *sync.Once
	restamper        PktRestamper
	statIncomingRate *astikit.CounterAvgStat
	statWorkRatio    *astikit.DurationPercentageStat
}

// MuxerOptions represents muxer options
type MuxerOptions struct {
	Format     *avformat.OutputFormat
	FormatName string
	Node       astiencoder.NodeOptions
	Restamper  PktRestamper
	URL        string
}

// NewMuxer creates a new muxer
func NewMuxer(o MuxerOptions, eh *astiencoder.EventHandler, c *astikit.Closer) (m *Muxer, err error) {
	// Extend node metadata
	count := atomic.AddUint64(&countMuxer, uint64(1))
	o.Node.Metadata = o.Node.Metadata.Extend(fmt.Sprintf("muxer_%d", count), fmt.Sprintf("Muxer #%d", count), fmt.Sprintf("Muxes to %s", o.URL))

	// Create muxer
	m = &Muxer{
		c: astikit.NewChan(astikit.ChanOptions{
			AddStrategy: astikit.ChanAddStrategyBlockWhenStarted,
			ProcessAll:  true,
		}),
		cl:               c,
		eh:               eh,
		o:                &sync.Once{},
		restamper:        o.Restamper,
		statIncomingRate: astikit.NewCounterAvgStat(),
		statWorkRatio:    astikit.NewDurationPercentageStat(),
	}
	m.BaseNode = astiencoder.NewBaseNode(o.Node, astiencoder.NewEventGeneratorNode(m), eh)
	m.addStats()

	// Alloc format context
	// We need to create an intermediate variable to avoid "cgo argument has Go pointer to Go pointer" errors
	var ctxFormat *avformat.Context
	if ret := avformat.AvformatAllocOutputContext2(&ctxFormat, o.Format, o.FormatName, o.URL); ret < 0 {
		err = fmt.Errorf("astilibav: avformat.AvformatAllocOutputContext2 on %+v failed: %w", o, NewAvError(ret))
		return
	}
	m.ctxFormat = ctxFormat

	// Make sure the format ctx is properly closed
	c.Add(func() error {
		m.ctxFormat.AvformatFreeContext()
		return nil
	})

	// This is a file
	if m.ctxFormat.Flags()&avformat.AVFMT_NOFILE == 0 {
		// Open
		var ctxAvIO *avformat.AvIOContext
		if ret := avformat.AvIOOpen(&ctxAvIO, o.URL, avformat.AVIO_FLAG_WRITE); ret < 0 {
			err = fmt.Errorf("astilibav: avformat.AvIOOpen on %+v failed: %w", o, NewAvError(ret))
			return
		}

		// Set pb
		m.ctxFormat.SetPb(ctxAvIO)

		// Make sure the avio ctx is properly closed
		c.Add(func() error {
			if ret := avformat.AvIOClosep(&ctxAvIO); ret < 0 {
				return fmt.Errorf("astilibav: avformat.AvIOClosep on %+v failed: %w", o, NewAvError(ret))
			}
			return nil
		})
	}
	return
}

func (m *Muxer) addStats() {
	// Add incoming rate
	m.Stater().AddStat(astikit.StatMetadata{
		Description: "Number of packets coming in per second",
		Label:       "Incoming rate",
		Unit:        "pps",
	}, m.statIncomingRate)

	// Add work ratio
	m.Stater().AddStat(astikit.StatMetadata{
		Description: "Percentage of time spent doing some actual work",
		Label:       "Work ratio",
		Unit:        "%",
	}, m.statWorkRatio)

	// Add chan stats
	m.c.AddStats(m.Stater())
}

// CtxFormat returns the format ctx
func (m *Muxer) CtxFormat() *avformat.Context {
	return m.ctxFormat
}

// Start starts the muxer
func (m *Muxer) Start(ctx context.Context, t astiencoder.CreateTaskFunc) {
	m.BaseNode.Start(ctx, t, func(t *astikit.Task) {
		// Make sure to write header once
		var ret int
		m.o.Do(func() { ret = m.ctxFormat.AvformatWriteHeader(nil) })
		if ret < 0 {
			emitAvError(m, m.eh, ret, "m.ctxFormat.AvformatWriteHeader on %s failed", m.ctxFormat.Filename())
			return
		}

		// Write trailer once everything is done
		m.cl.Add(func() error {
			if ret := m.ctxFormat.AvWriteTrailer(); ret < 0 {
				return fmt.Errorf("m.ctxFormat.AvWriteTrailer on %s failed: %w", m.ctxFormat.Filename(), NewAvError(ret))
			}
			return nil
		})

		// Make sure to stop the chan properly
		defer m.c.Stop()

		// Start chan
		m.c.Start(m.Context())
	})
}

// MuxerPktHandler is an object that can handle a pkt for the muxer
type MuxerPktHandler struct {
	*Muxer
	o *avformat.Stream
}

// NewHandler creates
func (m *Muxer) NewPktHandler(o *avformat.Stream) *MuxerPktHandler {
	return &MuxerPktHandler{
		Muxer: m,
		o:     o,
	}
}

// HandlePkt implements the PktHandler interface
func (h *MuxerPktHandler) HandlePkt(p *PktHandlerPayload) {
	h.c.Add(func() {
		// Handle pause
		defer h.HandlePause()

		// Increment incoming rate
		h.statIncomingRate.Add(1)

		// Rescale timestamps
		p.Pkt.AvPacketRescaleTs(p.Descriptor.TimeBase(), h.o.TimeBase())

		// Set stream index
		p.Pkt.SetStreamIndex(h.o.Index())

		// Restamp
		if h.restamper != nil {
			h.restamper.Restamp(p.Pkt)
		}

		// Write frame
		h.statWorkRatio.Begin()
		if ret := h.ctxFormat.AvInterleavedWriteFrame((*avformat.Packet)(unsafe.Pointer(p.Pkt))); ret < 0 {
			h.statWorkRatio.End()
			emitAvError(h, h.eh, ret, "h.ctxFormat.AvInterleavedWriteFrame failed")
			return
		}
		h.statWorkRatio.End()
	})
}
