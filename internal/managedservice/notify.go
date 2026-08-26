package managedservice

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// NotifyReady sends the one sd_notify readiness transition expected by the
// compiled Type=notify units. It is a no-op outside a supervised process.
func NotifyReady() error {
	socket, found := os.LookupEnv("NOTIFY_SOCKET")
	if !found {
		return nil
	}
	if err := os.Unsetenv("NOTIFY_SOCKET"); err != nil {
		return fmt.Errorf("clear NOTIFY_SOCKET: %w", err)
	}
	if strings.TrimSpace(socket) == "" {
		return nil
	}
	return notifyReady(socket)
}

func notifyReady(socket string) error {
	return notifyReadyWith(socket, func(address *net.UnixAddr) (io.WriteCloser, error) {
		return net.DialUnix("unixgram", nil, address)
	})
}

func notifyReadyWith(socket string, dial func(*net.UnixAddr) (io.WriteCloser, error)) error {
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	} else if !strings.HasPrefix(socket, "/") {
		return fmt.Errorf("NOTIFY_SOCKET must be an absolute or abstract Unix datagram address")
	}
	connection, err := dial(&net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("connect to systemd notification socket: %w", err)
	}
	defer connection.Close()
	message := []byte("READY=1")
	written, err := connection.Write(message)
	if err != nil {
		return fmt.Errorf("notify systemd readiness: %w", err)
	}
	if written != len(message) {
		return fmt.Errorf("notify systemd readiness: %w", io.ErrShortWrite)
	}
	return nil
}
