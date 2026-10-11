//go:build windows

package embed

import (
	"encoding/binary"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	afInet                   = 2
	tcpTableOwnerPIDListener = 3 // TCP_TABLE_OWNER_PID_LISTENER
)

// listenerPID returns the process id listening on 127.0.0.1:port (or 0.0.0.0:port).
// Findly uses it to make sure the server answering on a port is really the llama-server
// it started, not another program that grabbed the port first (plan P-4).
func listenerPID(port int) (uint32, error) {
	var size uint32
	// First call reports the buffer size needed.
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDListener, 0)
	for attempt := 0; attempt < 5; attempt++ {
		buf := make([]byte, size+1024)
		size = uint32(len(buf))
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, afInet, tcpTableOwnerPIDListener, 0)
		if r == uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			continue
		}
		if r != 0 {
			return 0, windows.Errno(r)
		}
		// MIB_TCPTABLE_OWNER_PID: DWORD count, then rows of 6 DWORDs:
		// state, localAddr, localPort, remoteAddr, remotePort, owningPid.
		n := binary.LittleEndian.Uint32(buf)
		const rowSize = 24
		for i := uint32(0); i < n; i++ {
			off := 4 + int(i)*rowSize
			if off+rowSize > len(buf) {
				break
			}
			row := buf[off : off+rowSize]
			addr := row[4:8] // in network order
			portBE := row[8:12]
			p := int(portBE[0])<<8 | int(portBE[1])
			loopbackOrAny := (addr[0] == 127 && addr[1] == 0 && addr[2] == 0 && addr[3] == 1) ||
				(addr[0] == 0 && addr[1] == 0 && addr[2] == 0 && addr[3] == 0)
			if p == port && loopbackOrAny {
				return binary.LittleEndian.Uint32(row[20:24]), nil
			}
		}
		return 0, errNoListener
	}
	return 0, errors.New("embed: TCP table kept growing")
}

var errNoListener = errors.New("embed: nobody is listening on the port")
