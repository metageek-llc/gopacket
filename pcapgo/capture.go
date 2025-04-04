// Copyright 2012 Google, Inc. All rights reserved.
//
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.
//go:build linux

package pcapgo

import (
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"

	"github.com/metageek-llc/gopacket"
	"github.com/metageek-llc/gopacket/endian"
)

var hdrLen = unix.CmsgSpace(0)
var auxLen = unix.CmsgSpace(int(unsafe.Sizeof(unix.TpacketAuxdata{})))
var timensLen = unix.CmsgSpace(int(unsafe.Sizeof(unix.Timespec{})))
var timeLen = unix.CmsgSpace(int(unsafe.Sizeof(unix.Timeval{})))

// EthernetHandle holds shared buffers and file descriptor of af_packet socket
type EthernetHandle struct {
	file   *os.File
	buffer []byte
	oob    []byte
	ancil  []interface{}
	mu     sync.Mutex
	intf   int
	addr   net.HardwareAddr
}

// readOne reads a packet from the handle and returns a capture info + vlan info
func (h *EthernetHandle) readOne() (ci gopacket.CaptureInfo, vlan int, haveVlan bool, err error) {
	// we could use unix.Recvmsg, but that does a memory allocation (for the returned sockaddr) :(
	var msg unix.Msghdr
	var sa unix.RawSockaddrLinklayer

	msg.Name = (*byte)(unsafe.Pointer(&sa))
	msg.Namelen = uint32(unsafe.Sizeof(sa))

	var iov unix.Iovec
	if len(h.buffer) > 0 {
		iov.Base = &h.buffer[0]
		iov.SetLen(len(h.buffer))
	}
	msg.Iov = &iov
	msg.Iovlen = 1

	if len(h.oob) > 0 {
		msg.Control = &h.oob[0]
		msg.SetControllen(len(h.oob))
	}

	rawConn, err := h.file.SyscallConn()
	if err != nil {
		return
	}

	var (
		e syscall.Errno
		n uintptr
	)
	err = rawConn.Read(func(fd uintptr) bool {
		// use msg_trunc so we know packet size without auxdata, which might be missing
		n, _, e = syscall.Syscall(unix.SYS_RECVMSG, fd, uintptr(unsafe.Pointer(&msg)), uintptr(unix.MSG_TRUNC))
		switch e {
		case 0:
			return true
		case syscall.EAGAIN:
			return false
		default:
			// some other error
			return true
		}
	})
	if err != nil {
		return
	}

	switch {
	case e.Temporary() || e.Timeout():
		return ci, 0, false, e
	case e != 0:
		return ci, 0, false, fmt.Errorf("couldn't read packet: %w", e)
	}

	if sa.Family == unix.AF_PACKET {
		ci.InterfaceIndex = int(sa.Ifindex)
	} else {
		ci.InterfaceIndex = h.intf
	}

	// custom aux parsing so we don't allocate stuff (unix.ParseSocketControlMessage allocates a slice)
	// we're getting at most 2 cmsgs anyway and know which ones they are (auxdata + timestamp(ns))
	oob := h.oob[:msg.Controllen]
	gotAux := false

	for len(oob) > hdrLen { // > hdrLen, because we also need something after the cmsg header
		hdr := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
		switch {
		case hdr.Level == unix.SOL_PACKET && hdr.Type == unix.PACKET_AUXDATA && len(oob) >= auxLen:
			aux := (*unix.TpacketAuxdata)(unsafe.Pointer(&oob[hdrLen]))
			ci.CaptureLength = int(n)
			ci.Length = int(aux.Len)
			vlan = int(aux.Vlan_tci)
			haveVlan = (aux.Status & unix.TP_STATUS_VLAN_VALID) != 0
			gotAux = true
		case hdr.Level == unix.SOL_SOCKET && hdr.Type == unix.SO_TIMESTAMPNS && len(oob) >= timensLen:
			tstamp := (*unix.Timespec)(unsafe.Pointer(&oob[hdrLen]))
			ci.Timestamp = time.Unix(int64(tstamp.Sec), int64(tstamp.Nsec))
		case hdr.Level == unix.SOL_SOCKET && hdr.Type == unix.SO_TIMESTAMP && len(oob) >= timeLen:
			tstamp := (*unix.Timeval)(unsafe.Pointer(&oob[hdrLen]))
			ci.Timestamp = time.Unix(int64(tstamp.Sec), int64(tstamp.Usec)*1000)
		}
		oob = oob[unix.CmsgSpace(int(hdr.Len))-hdrLen:]
	}

	if !gotAux {
		// fallback for no aux cmsg
		ci.CaptureLength = int(n)
		ci.Length = int(n)
		haveVlan = false
	}

	// fix up capture length if we needed to truncate
	if ci.CaptureLength > len(h.buffer) {
		ci.CaptureLength = len(h.buffer)
	}

	if ci.Timestamp.IsZero() {
		// we got no timestamp info -> emulate it
		ci.Timestamp = time.Now()
	}

	return ci, vlan, haveVlan, nil
}

// ReadPacketData implements gopacket.PacketDataSource. If this was captured on a vlan, the vlan id will be in the AncillaryData[0]
func (h *EthernetHandle) ReadPacketData() ([]byte, gopacket.CaptureInfo, error) {
	h.mu.Lock()
	ci, vlan, haveVlan, err := h.readOne()
	if err != nil {
		h.mu.Unlock()
		return nil, gopacket.CaptureInfo{}, fmt.Errorf("couldn't read packet data: %s", err)
	}

	b := make([]byte, ci.CaptureLength)
	copy(b, h.buffer)
	h.mu.Unlock()

	if haveVlan {
		ci.AncillaryData = []interface{}{vlan}

	}

	return b, ci, nil
}

// ZeroCopyReadPacketData implements gopacket.ZeroCopyPacketDataSource. If this was captured on a vlan, the vlan id will be in the AncillaryData[0].
// This function does not allocate memory. Beware that the next call to ZeroCopyReadPacketData will overwrite existing slices (returned data AND AncillaryData)!
// Due to shared buffers this must not be called concurrently
func (h *EthernetHandle) ZeroCopyReadPacketData() ([]byte, gopacket.CaptureInfo, error) {
	ci, vlan, haveVlan, err := h.readOne()
	if err != nil {
		return nil, gopacket.CaptureInfo{}, fmt.Errorf("couldn't read packet data: %s", err)
	}

	if haveVlan {
		h.ancil[0] = vlan
		ci.AncillaryData = h.ancil
	}

	return h.buffer[:ci.CaptureLength], ci, nil
}

// Close closes the underlying socket
func (h *EthernetHandle) Close() error {
	return h.file.Close()
}

// SetCaptureLength sets the maximum capture length to the given value
func (h *EthernetHandle) SetCaptureLength(len int) error {
	if len < 0 {
		return fmt.Errorf("illegal capture length %d. Must be at least 0", len)
	}
	h.buffer = make([]byte, len)
	return nil
}

// GetCaptureLength returns the maximum capture length
func (h *EthernetHandle) GetCaptureLength() int {
	return len(h.buffer)
}

// SetBPF attaches the given BPF filter to the socket. After this, only the packets for which the filter returns a value greater than zero are received.
// If a filter was already attached, it will be overwritten. To remove the filter, provide an empty slice.
func (h *EthernetHandle) SetBPF(filter []bpf.RawInstruction) (err error) {
	rawConn, err := h.file.SyscallConn()
	if err != nil {
		return
	}

	err2 := rawConn.Control(func(fd uintptr) {
		if len(filter) == 0 {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DETACH_FILTER, 0)
			return
		}
		f := make([]unix.SockFilter, len(filter))
		for i := range filter {
			f[i].Code = filter[i].Op
			f[i].Jf = filter[i].Jf
			f[i].Jt = filter[i].Jt
			f[i].K = filter[i].K
		}
		fprog := &unix.SockFprog{
			Len:    uint16(len(filter)),
			Filter: &f[0],
		}
		err = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, fprog)
	})
	if err2 != nil {
		return err2
	}
	return
}

// LocalAddr returns the local network address
func (h *EthernetHandle) LocalAddr() net.HardwareAddr {
	// Hardware Address might have changed. Fetch new one and fall back to the stored one if fetching interface fails
	intf, err := net.InterfaceByIndex(h.intf)
	if err == nil {
		h.addr = intf.HardwareAddr
	}
	return h.addr
}

// SetPromiscuous sets promiscous mode to the required value. If it is enabled, traffic not destined for the interface will also be captured.
func (h *EthernetHandle) SetPromiscuous(b bool) (err error) {
	rawConn, err := h.file.SyscallConn()
	if err != nil {
		return
	}

	mreq := unix.PacketMreq{
		Ifindex: int32(h.intf),
		Type:    unix.PACKET_MR_PROMISC,
	}

	opt := unix.PACKET_ADD_MEMBERSHIP
	if !b {
		opt = unix.PACKET_DROP_MEMBERSHIP
	}

	err2 := rawConn.Control(func(fd uintptr) {
		err = unix.SetsockoptPacketMreq(int(fd), unix.SOL_PACKET, opt, &mreq)
	})
	if err2 != nil {
		return err2
	}
	return
}

// Stats returns number of packets and dropped packets. This will be the number of packets/dropped packets since the last call to stats (not the cummulative sum!).
func (h *EthernetHandle) Stats() (res *unix.TpacketStats, err error) {
	rawConn, err := h.file.SyscallConn()
	if err != nil {
		return
	}
	err2 := rawConn.Control(func(fd uintptr) {
		res, err = unix.GetsockoptTpacketStats(int(fd), unix.SOL_PACKET, unix.PACKET_STATISTICS)
	})
	if err2 != nil {
		return nil, err2
	}
	return
}

// NewEthernetHandle implements pcap.OpenLive for network devices.
// If you want better performance have a look at github.com/metageek-llc/gopacket/afpacket.
// SetCaptureLength can be used to limit the maximum capture length.
func NewEthernetHandle(ifname string) (*EthernetHandle, error) {
	intf, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("couldn't query interface %s: %s", ifname, err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(endian.Htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("couldn't open packet socket: %s", err)
	}

	addr := unix.SockaddrLinklayer{
		Protocol: endian.Htons(unix.ETH_P_ALL),
		Ifindex:  intf.Index,
	}

	if err := unix.Bind(fd, &addr); err != nil {
		return nil, fmt.Errorf("couldn't bind to interface %s: %s", ifname, err)
	}

	ooblen := 0

	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1); err != nil {
		// we can't get auxdata -> no vlan info
	} else {
		ooblen += auxLen
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		// no nanosecond resolution :( -> try ms
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMP, 1); err != nil {
			// if this doesn't work we well use time.Now() -> ignore errors here
		} else {
			ooblen += timeLen
		}
	} else {
		ooblen += timensLen
	}

	handle := &EthernetHandle{
		file:   os.NewFile(uintptr(fd), ""),
		buffer: make([]byte, intf.MTU),
		oob:    make([]byte, ooblen),
		ancil:  make([]interface{}, 1),
		intf:   intf.Index,
		addr:   intf.HardwareAddr,
	}
	return handle, nil
}
