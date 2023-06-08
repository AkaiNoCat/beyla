package httpfltr

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	ebpfcommon "github.com/grafana/ebpf-autoinstrument/pkg/ebpf/common"
	"github.com/grafana/ebpf-autoinstrument/pkg/exec"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/exp/slog"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/grafana/ebpf-autoinstrument/pkg/goexec"
)

//go:generate $BPF2GO -cc $BPF_CLANG -cflags $BPF_CFLAGS -target amd64,arm64 bpf ../../../bpf/http_sock.c -- -I../../../bpf/headers
//go:generate $BPF2GO -cc $BPF_CLANG -cflags $BPF_CFLAGS -target amd64,arm64 bpf_debug ../../../bpf/http_sock.c -- -I../../../bpf/headers -DBPF_DEBUG

var activePids, _ = lru.New[uint32, string](64)

type BPFHTTPInfo bpfHttpInfoT
type BPFConnInfo bpfConnectionInfoT

type HTTPInfo struct {
	BPFHTTPInfo
	Method string
	URL    string
	Comm   string
	Host   string
	Peer   string
}

type Tracer struct {
	Cfg        *ebpfcommon.TracerConfig
	bpfObjects bpfObjects
	closers    []io.Closer
}

func logger() *slog.Logger {
	return slog.With("component", "httpfltr.Tracer")
}

func (p *Tracer) Load() (*ebpf.CollectionSpec, error) {
	loader := loadBpf
	if p.Cfg.BpfDebug {
		loader = loadBpf_debug
	}
	return loader()
}

func (p *Tracer) Constants(finfo *exec.FileInfo, _ *goexec.Offsets) map[string]any {
	if p.Cfg.SystemWide {
		return nil
	}

	m := map[string]any{"current_pid": finfo.Pid}

	npid, err := findNamespace(finfo.Pid)
	if err != nil {
		logger().Warn("error while looking up namespace pid, namespace pid matching will not work", err)
	}

	m["current_pid_ns_id"] = npid

	return m
}

func (p *Tracer) BpfObjects() any {
	return &p.bpfObjects
}

func (p *Tracer) AddCloser(c ...io.Closer) {
	p.closers = append(p.closers, c...)
}

func (p *Tracer) GoProbes() map[string]ebpfcommon.FunctionPrograms {
	return nil
}

func (p *Tracer) KProbes() map[string]ebpfcommon.FunctionPrograms {
	kprobes := map[string]ebpfcommon.FunctionPrograms{
		// Both sys accept probes use the same kretprobe.
		// We could tap into __sys_accept4, but we might be more prone to
		// issues with the internal kernel code changing.
		"sys_accept": {
			Required: true,
			End:      p.bpfObjects.KretprobeSysAccept4,
		},
		"sys_accept4": {
			Required: true,
			End:      p.bpfObjects.KretprobeSysAccept4,
		},
		"sock_alloc": {
			Required: true,
			End:      p.bpfObjects.KretprobeSockAlloc,
		},
		"tcp_rcv_established": {
			Required: true,
			Start:    p.bpfObjects.KprobeTcpRcvEstablished,
		},
		// Tracking of HTTP client calls, by tapping into connect
		"sys_connect": {
			Required: true,
			End:      p.bpfObjects.KretprobeSysConnect,
		},
		"tcp_connect": {
			Required: true,
			Start:    p.bpfObjects.KprobeTcpConnect,
		},
	}

	// Track system exit so we can find program names of dead programs
	// when we process the events
	if p.Cfg.SystemWide {
		kprobes["sys_exit"] = ebpfcommon.FunctionPrograms{
			Required: true,
			Start:    p.bpfObjects.KprobeSysExit,
		}
		kprobes["sys_exit_group"] = ebpfcommon.FunctionPrograms{
			Required: true,
			Start:    p.bpfObjects.KprobeSysExit,
		}
	}

	return kprobes
}

func (p *Tracer) SocketFilters() []*ebpf.Program {
	return []*ebpf.Program{p.bpfObjects.SocketHttpFilter}
}

func (p *Tracer) Run(ctx context.Context, eventsChan chan<- []any) {
	ebpfcommon.ForwardRingbuf(
		p.Cfg, logger(), p.bpfObjects.Events, p.toRequestTrace,
		append(p.closers, &p.bpfObjects)...,
	)(ctx, eventsChan)
}

func (p *Tracer) toRequestTrace(record *ringbuf.Record) (any, error) {
	var event BPFHTTPInfo
	var result HTTPInfo

	err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
	if err != nil {
		return result, err
	}

	result = HTTPInfo{BPFHTTPInfo: event}

	source, target := event.hostInfo()
	result.Host = target
	result.Peer = source
	result.URL = event.url()
	result.Method = event.method()
	if p.Cfg.SystemWide {
		result.Comm = p.serviceName(event.Pid)
	}

	return result, nil
}

func (event *BPFHTTPInfo) url() string {
	buf := string(event.Buf[:])
	space := strings.Index(buf, " ")
	if space < 0 {
		return ""
	}
	nextSpace := strings.Index(buf[space+1:], " ")
	if nextSpace < 0 {
		return ""
	}

	return buf[space+1 : nextSpace+space+1]
}

func (event *BPFHTTPInfo) method() string {
	buf := string(event.Buf[:])
	space := strings.Index(buf, " ")
	if space < 0 {
		return ""
	}

	return buf[:space]
}

func (event *BPFHTTPInfo) hostInfo() (source, target string) {
	src := make(net.IP, net.IPv6len)
	dst := make(net.IP, net.IPv6len)
	copy(src, event.ConnInfo.S_addr[:])
	copy(dst, event.ConnInfo.D_addr[:])

	return src.String(), dst.String()
}

func cstr(chars []uint8) string {
	addrLen := bytes.IndexByte(chars[:], 0)
	if addrLen < 0 {
		addrLen = len(chars)
	}

	return string(chars[:addrLen])
}

func (p *Tracer) commNameOfDeadPid(pid uint32) string {
	var name [16]uint8
	err := p.bpfObjects.DeadPids.Lookup(pid, &name)
	if err != nil {
		return ""
	}

	return cstr(name[:])
}

func (p *Tracer) commName(pid uint32) string {
	procPath := filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "comm")
	_, err := os.Stat(procPath)
	if os.IsNotExist(err) {
		return p.commNameOfDeadPid(pid)
	}

	name, err := os.ReadFile(procPath)
	if err != nil {
		p.commNameOfDeadPid(pid)
	}

	return strings.TrimSpace(string(name))
}

func (p *Tracer) serviceName(pid uint32) string {
	cached, ok := activePids.Get(pid)
	if ok {
		return cached
	}

	name := p.commName(pid)
	activePids.Add(pid, name)
	return name
}