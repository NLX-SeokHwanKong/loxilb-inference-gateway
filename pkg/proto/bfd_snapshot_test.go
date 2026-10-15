package bfd

import (
	"net"
	"testing"
	"time"
)

func TestSnapshotBFDRegistersBeforeTrafficGate(t *testing.T) {
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ready := make(chan struct{})
	bs := &Struct{BFDSessMap: make(map[string]*bfdSession)}
	args := ConfigArgs{RemoteIP: "127.0.0.1", SourceIP: "127.0.0.1", Port: uint16(peer.LocalAddr().(*net.UDPAddr).Port), Interval: 100000, Multi: 5, Instance: "test", StartGate: ready}
	if err := bs.BFDAddRemote(args, nil); err != nil {
		t.Fatal(err)
	}
	defer bs.BFDDeleteRemote(args)
	got, err := bs.BFDGet()
	if err != nil || len(got) != 1 || got[0].Interval != uint64(args.Interval) {
		t.Fatalf("accepted configuration missing: %+v %v", got, err)
	}
	if bs.BFDSessMap[args.RemoteIP].active() {
		t.Fatal("protocol active before synchronization")
	}
	if err := bs.BFDAddRemote(args, nil); err != nil {
		t.Fatalf("pending retry: %v", err)
	}
	buffer := make([]byte, 64)
	peer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := peer.ReadFrom(buffer); err == nil {
		t.Fatal("traffic escaped the synchronization gate")
	}
	close(ready)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if n, _, err := peer.ReadFrom(buffer); err != nil || n != 24 {
		t.Fatalf("protocol did not start after synchronization: n=%d err=%v", n, err)
	}
}

func TestSnapshotBFDCanDeleteWhileStartIsPending(t *testing.T) {
	ready := make(chan struct{})
	defer close(ready)
	bs := &Struct{BFDSessMap: make(map[string]*bfdSession)}
	args := ConfigArgs{RemoteIP: "127.0.0.2", SourceIP: "127.0.0.1", Port: 3784, Interval: 100000, Multi: 5, Instance: "test", StartGate: ready}
	if err := bs.BFDAddRemote(args, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- bs.BFDDeleteRemote(args) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete blocked on a pending protocol start")
	}
	if len(bs.BFDSessMap) != 0 {
		t.Fatal("deleted pending session retained")
	}
}
