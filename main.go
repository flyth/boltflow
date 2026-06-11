// boltflow - make the mouse follow the keyboard across hosts (Logi Bolt).
//
// `run` diverts the keyboard's Easy-Switch keys (HID++ 0x1B04), so pressing
// one delivers the keypress here instead of switching the keyboard. The agent
// then moves the mouse AND the keyboard to the chosen host (HID++ 0x1814).
// Everything is local to the host the devices are currently connected to —
// no network involved.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sstallion/go-hid"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: boltflow <command>

Commands:
  list           Show devices on the Bolt receiver, their host channels and
                 whether their Easy-Switch keys can be diverted
  switch <1-3>   Move keyboard and mouse to the given host channel now
  run            Follow mode: keyboard Easy-Switch keys also move the mouse
`)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if err := hid.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "hid init:", err)
		os.Exit(1)
	}
	defer hid.Exit()

	rcv, err := OpenBoltReceiver()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rcv.Close()

	devs := rcv.Discover()
	if len(devs) == 0 {
		fmt.Fprintln(os.Stderr, "no devices answered on the Bolt receiver (asleep? move the mouse)")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "list":
		cmdList(devs)
	case "switch":
		if len(os.Args) != 3 {
			usage()
		}
		var host int
		fmt.Sscanf(os.Args[2], "%d", &host)
		if host < 1 || host > 3 {
			usage()
		}
		cmdSwitch(devs, byte(host-1))
	case "run":
		cmdRun(rcv, devs)
	case "sniff":
		cmdSniff(rcv)
	default:
		usage()
	}
}

// cmdSniff dumps every report arriving on the receiver's HID++ interface —
// used to discover what (if anything) the keyboard sends to the departing
// host when a real Easy-Switch key is pressed.
func cmdSniff(rcv *Receiver) {
	fmt.Println("sniffing HID++ reports (ctrl-c to stop)...")
	for {
		buf := make([]byte, 64)
		n, err := rcv.dev.ReadWithTimeout(buf, 60*time.Second)
		if err != nil || n == 0 {
			continue
		}
		fmt.Printf("[%s] % X\n", time.Now().Format("15:04:05.000"), buf[:n])
	}
}

func cmdList(devs []*Device) {
	for _, d := range devs {
		fmt.Printf("[%d] %s (%s)", d.Index, d.Name, TypeName(d.Type))
		if d.NumHost > 0 {
			fmt.Printf("  host %d/%d", d.CurHost+1, d.NumHost)
		}
		if idx, err := d.featureIndex(featChangeHost); err == nil {
			fmt.Printf("  [0x1814@0x%02X]", idx)
		}
		fmt.Println()
		ctrls, err := d.Controls()
		if err != nil {
			fmt.Printf("    controls: %v\n", err)
			continue
		}
		for _, c := range ctrls {
			div := "not divertable"
			if c.Divertable() {
				div = "divertable"
			}
			label := ""
			if c.CID >= cidHost1 && c.CID <= cidHost3 {
				label = fmt.Sprintf(" (easy-switch key %d)", c.CID-cidHost1+1)
			}
			fmt.Printf("    cid 0x%04X task 0x%04X flags 0x%02X (%s)%s\n",
				c.CID, c.Task, c.Flags, div, label)
		}
	}
}

func cmdSwitch(devs []*Device, host byte) {
	// Keyboard last: while the keyboard is still here, a failed mouse command
	// is visible and recoverable; once the keyboard jumps, this host is done.
	for _, d := range devs {
		if !d.IsKeyboard() {
			moveDevice(d, host)
		}
	}
	for _, d := range devs {
		if d.IsKeyboard() {
			moveDevice(d, host)
		}
	}
}

func moveDevice(d *Device, host byte) {
	// Refresh host state first: it also tells us whether the device is even
	// reachable on this receiver right now (it may live on another host).
	if err := d.loadHostInfo(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: not reachable here (%v), skipping\n", d.Name, err)
		return
	}
	if d.CurHost == host {
		fmt.Printf("%s already on host %d\n", d.Name, host+1)
		return
	}
	if err := d.SetHost(host); err != nil {
		fmt.Fprintf(os.Stderr, "%s: change host failed: %v\n", d.Name, err)
		return
	}
	d.CurHost = host
	fmt.Printf("%s -> host %d\n", d.Name, host+1)
}

// True follow mode. When an Easy-Switch key is pressed, the device announces
// the change to the host it is LEAVING as an unsolicited HID++ event on its
// Change Host feature (0x1814): `11 <devIdx> <idx1814> 00 <p0> <targetHost>`.
// This is not in Logitech's public feature docs (and the Easy-Switch keys are
// not divertable), but it is what Options+ uses for its follow behavior — and
// it is plainly observable with `boltflow sniff`. We listen for it from every
// device and chase with all the others. Fully local, keys stay native.
func cmdRun(rcv *Receiver, devs []*Device) {
	byIdx := map[byte]*Device{}
	idx1814 := map[byte]byte{}
	fmt.Print("following host changes of:")
	for _, d := range devs {
		if d.NumHost == 0 {
			continue
		}
		idx, err := d.featureIndex(featChangeHost)
		if err != nil {
			continue
		}
		byIdx[d.Index] = d
		idx1814[d.Index] = idx
		fmt.Printf(" %s", d.Name)
	}
	fmt.Println()
	if len(byIdx) < 2 {
		fmt.Fprintln(os.Stderr, "need at least two host-switchable devices to follow")
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		rcv.Close()
		hid.Exit()
		os.Exit(0)
	}()

	var lastSwitch time.Time
	for {
		buf := make([]byte, 64)
		n, err := rcv.dev.ReadWithTimeout(buf, 60*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			time.Sleep(time.Second)
			continue
		}
		if n < 6 || buf[0] != reportLong {
			continue
		}
		src, ok := byIdx[buf[1]]
		if !ok || buf[2] != idx1814[buf[1]] || buf[3] != 0x00 {
			continue // not a Change Host event (byte3: event 0, swid 0)
		}
		target := buf[5]
		if time.Since(lastSwitch) < time.Second {
			continue // debounce
		}
		lastSwitch = time.Now()
		fmt.Printf("%s is switching to host %d: bringing the others\n", src.Name, target+1)
		for _, d := range byIdx {
			if d != src {
				moveDevice(d, target)
			}
		}
	}
}
