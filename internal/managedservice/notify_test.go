package managedservice

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
)

func TestNotifyReadySendsTheSystemdTransition(t *testing.T) {
	for name, socket := range map[string]string{"abstract": "@dorf-ready", "filesystem": "/run/systemd/notify"} {
		t.Run(name, func(t *testing.T) {
			connection := &recordingNotifyConnection{}
			var address *net.UnixAddr
			if err := notifyReadyWith(socket, func(got *net.UnixAddr) (io.WriteCloser, error) {
				address = got
				return connection, nil
			}); err != nil {
				t.Fatal(err)
			}
			want := socket
			if socket[0] == '@' {
				want = "\x00" + socket[1:]
			}
			if address == nil || address.Net != "unixgram" || address.Name != want {
				t.Fatalf("address=%#v", address)
			}
			if got := connection.String(); got != "READY=1" || !connection.closed {
				t.Fatalf("notification=%q closed=%t", got, connection.closed)
			}
		})
	}
}

type recordingNotifyConnection struct {
	bytes.Buffer
	closed bool
}

func (connection *recordingNotifyConnection) Close() error {
	connection.closed = true
	return nil
}

func TestNotifyReadyIsANoopWithoutSupervision(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := NotifyReady(); err != nil {
		t.Fatal(err)
	}
	if _, found := os.LookupEnv("NOTIFY_SOCKET"); found {
		t.Fatal("captured NOTIFY_SOCKET remained in the process environment")
	}
}

func TestNotifyReadyClearsCapturedSocketBeforeReportingAnError(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "relative.sock")
	if err := NotifyReady(); err == nil {
		t.Fatal("relative notification socket was accepted")
	}
	if _, found := os.LookupEnv("NOTIFY_SOCKET"); found {
		t.Fatal("failed notification left NOTIFY_SOCKET in the environment")
	}
}

func TestNotifyReadyReportsDialAndShortWrite(t *testing.T) {
	dialErr := errors.New("dial failed")
	err := notifyReadyWith("/run/systemd/notify", func(*net.UnixAddr) (io.WriteCloser, error) {
		return nil, dialErr
	})
	if !errors.Is(err, dialErr) {
		t.Fatalf("dial error=%v", err)
	}
	err = notifyReadyWith("/run/systemd/notify", func(*net.UnixAddr) (io.WriteCloser, error) {
		return shortNotifyConnection{}, nil
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short-write error=%v", err)
	}
}

type shortNotifyConnection struct{}

func (shortNotifyConnection) Write(payload []byte) (int, error) { return len(payload) - 1, nil }
func (shortNotifyConnection) Close() error                      { return nil }
