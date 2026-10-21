package boxexec

import (
	"context"

	"github.com/coder/websocket"
)

// Proxy copies messages between a and b, both ways, until one side closes,
// and passes a normal close on to the other side.
func Proxy(ctx context.Context, a, b *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- forward(ctx, a, b) }()
	go func() { errc <- forward(ctx, b, a) }()
	err := <-errc
	cancel()
	if err2 := <-errc; err == nil {
		err = err2
	}
	return err
}

func forward(ctx context.Context, dst, src *websocket.Conn) error {
	r := &messageReader{conn: src}
	for {
		typ, data, err := r.read(ctx)
		if status := websocket.CloseStatus(err); status != -1 {
			_ = dst.Close(status, "")
			return nil
		}
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}
