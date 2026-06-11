package main

// Minimal HID++ 2.0 implementation, just enough to talk to devices behind a
// Logi Bolt receiver: ping, feature lookup, device name/type, Change Host
// (0x1814) and Reprogrammable Controls v4 (0x1B04) for diverting the
// Easy-Switch keys.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sstallion/go-hid"
)

const (
	vidLogitech = 0x046D
	pidBolt     = 0xC548

	reportShort = 0x10 // 7 bytes
	reportLong  = 0x11 // 20 bytes

	swID = 0x0A // our software id nibble

	featRoot        = 0x0000
	featDeviceName  = 0x0005
	featChangeHost  = 0x1814
	featReprogCtrls = 0x1B04

	// Easy-Switch / host switch control IDs (Solaar special_keys.py)
	cidHost1 = 0x00D1
	cidHost2 = 0x00D2
	cidHost3 = 0x00D3
)

type Receiver struct {
	dev *hid.Device
}

type Device struct {
	rcv     *Receiver
	Index   byte
	Name    string
	Type    byte // 0=keyboard 3=mouse (feature 0x0005 fn2)
	feat    map[uint16]byte
	NumHost byte
	CurHost byte // 0-based
}

func (d *Device) IsKeyboard() bool { return d.Type == 0 }
func (d *Device) IsMouse() bool    { return d.Type == 3 }

func TypeName(t byte) string {
	switch t {
	case 0:
		return "keyboard"
	case 3:
		return "mouse"
	case 4:
		return "trackpad"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

// OpenBoltReceiver opens the HID++ vendor interface (usage page 0xFF00) of
// the first Bolt receiver found.
func OpenBoltReceiver() (*Receiver, error) {
	var path string
	hid.Enumerate(vidLogitech, pidBolt, func(info *hid.DeviceInfo) error {
		if info.UsagePage == 0xFF00 && path == "" {
			path = info.Path
		}
		return nil
	})
	if path == "" {
		return nil, errors.New("no Logi Bolt receiver (046d:c548, usage page 0xFF00) found")
	}
	dev, err := hid.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (macOS: grant Input Monitoring to your terminal?)", path, err)
	}
	return &Receiver{dev: dev}, nil
}

func (r *Receiver) Close() { r.dev.Close() }

// request sends a long HID++ report and waits for the matching reply
// (same device index, feature index and function/swid byte), skipping
// unrelated traffic (notifications, diverted keys). HID++ errors come back
// as subid 0x8F (short) / 0xFF (long).
func (r *Receiver) request(devIdx, featIdx, fnID byte, params ...byte) ([]byte, error) {
	buf := make([]byte, 20)
	buf[0] = reportLong
	buf[1] = devIdx
	buf[2] = featIdx
	buf[3] = fnID<<4 | swID
	copy(buf[4:], params)
	var werr error
	for attempt := 0; attempt < 3; attempt++ {
		if _, werr = r.dev.Write(buf); werr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond) // transient IOHIDDeviceSetReport errors under traffic
	}
	if werr != nil {
		return nil, werr
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp := make([]byte, 64)
		n, err := r.dev.ReadWithTimeout(resp, time.Until(deadline))
		if err != nil || n == 0 {
			continue
		}
		resp = resp[:n]
		if resp[1] != devIdx {
			continue
		}
		// error replies: [0x10/0x11, devIdx, 0x8F/0xFF, featIdx, fnID|swID, errCode]
		if (resp[0] == reportShort && resp[2] == 0x8F) || (resp[0] == reportLong && resp[2] == 0xFF) {
			if resp[3] == featIdx && resp[4] == fnID<<4|swID {
				return nil, fmt.Errorf("hid++ error 0x%02X (feat 0x%02X fn %d)", resp[5], featIdx, fnID)
			}
			continue
		}
		if resp[0] == reportLong && resp[2] == featIdx && resp[3] == fnID<<4|swID {
			return resp[4:], nil
		}
	}
	return nil, fmt.Errorf("timeout waiting for reply (dev %d feat 0x%02X fn %d)", devIdx, featIdx, fnID)
}

// Ping uses root function 1; a connected HID++ 2.x device echoes the marker.
func (r *Receiver) Ping(devIdx byte) bool {
	resp, err := r.request(devIdx, 0x00, 0x1, 0x00, 0x00, 0xAA)
	return err == nil && len(resp) >= 3 && resp[2] == 0xAA
}

func (d *Device) featureIndex(featID uint16) (byte, error) {
	if idx, ok := d.feat[featID]; ok {
		return idx, nil
	}
	resp, err := d.rcv.request(d.Index, 0x00, 0x0, byte(featID>>8), byte(featID))
	if err != nil {
		return 0, err
	}
	if resp[0] == 0 {
		return 0, fmt.Errorf("feature 0x%04X not supported", featID)
	}
	d.feat[featID] = resp[0]
	return resp[0], nil
}

func (d *Device) loadNameAndType() error {
	idx, err := d.featureIndex(featDeviceName)
	if err != nil {
		return err
	}
	resp, err := d.rcv.request(d.Index, idx, 0x0)
	if err != nil {
		return err
	}
	count := int(resp[0])
	var sb strings.Builder
	for sb.Len() < count {
		chunk, err := d.rcv.request(d.Index, idx, 0x1, byte(sb.Len()))
		if err != nil {
			return err
		}
		for _, c := range chunk {
			if c == 0 || sb.Len() >= count {
				break
			}
			sb.WriteByte(c)
		}
	}
	d.Name = sb.String()
	if resp, err = d.rcv.request(d.Index, idx, 0x2); err == nil {
		d.Type = resp[0]
	}
	return nil
}

func (d *Device) loadHostInfo() error {
	idx, err := d.featureIndex(featChangeHost)
	if err != nil {
		return err
	}
	resp, err := d.rcv.request(d.Index, idx, 0x0)
	if err != nil {
		return err
	}
	d.NumHost, d.CurHost = resp[0], resp[1]
	return nil
}

// SetHost moves the device to host channel `host` (0-based). The device
// disconnects from this receiver as a result, so no reply is expected.
func (d *Device) SetHost(host byte) error {
	idx, err := d.featureIndex(featChangeHost)
	if err != nil {
		return err
	}
	buf := make([]byte, 20)
	buf[0] = reportLong
	buf[1] = d.Index
	buf[2] = idx
	buf[3] = 0x1<<4 | swID
	buf[4] = host
	_, err = d.rcv.dev.Write(buf)
	return err
}

type Control struct {
	CID, Task uint16
	Flags     byte
}

func (c Control) Divertable() bool { return c.Flags&0x20 != 0 }

func (d *Device) Controls() ([]Control, error) {
	idx, err := d.featureIndex(featReprogCtrls)
	if err != nil {
		return nil, err
	}
	resp, err := d.rcv.request(d.Index, idx, 0x0)
	if err != nil {
		return nil, err
	}
	count := int(resp[0])
	ctrls := make([]Control, 0, count)
	for i := 0; i < count; i++ {
		ci, err := d.rcv.request(d.Index, idx, 0x1, byte(i))
		if err != nil {
			return nil, err
		}
		ctrls = append(ctrls, Control{
			CID:   uint16(ci[0])<<8 | uint16(ci[1]),
			Task:  uint16(ci[2])<<8 | uint16(ci[3]),
			Flags: ci[4],
		})
	}
	return ctrls, nil
}

// SetDivert turns diversion of a control on/off (volatile; resets when the
// device reconnects or power-cycles).
func (d *Device) SetDivert(cid uint16, divert bool) error {
	idx, err := d.featureIndex(featReprogCtrls)
	if err != nil {
		return err
	}
	flags := byte(0x02) // dvalid
	if divert {
		flags |= 0x01
	}
	_, err = d.rcv.request(d.Index, idx, 0x3, byte(cid>>8), byte(cid), flags)
	return err
}

// Discover probes device indexes 1..6 behind the receiver and returns the
// HID++ 2.x devices that answer.
func (r *Receiver) Discover() []*Device {
	var devs []*Device
	for idx := byte(1); idx <= 6; idx++ {
		if !r.Ping(idx) {
			continue
		}
		d := &Device{rcv: r, Index: idx, feat: map[uint16]byte{}}
		if err := d.loadNameAndType(); err != nil {
			continue
		}
		d.loadHostInfo() // optional; NumHost stays 0 if unsupported
		devs = append(devs, d)
	}
	return devs
}
