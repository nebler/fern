package boxexec

import (
	"context"

	"github.com/coder/websocket"
)

// Proxy copies messages between a and b, both ways, until either side ends,
// and passes a close status on to the other side.
func Proxy(ctx context.Context, a, b *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() { forward(ctx, a, b); done <- struct{}{} }()
	go func() { forward(ctx, b, a); done <- struct{}{} }()
	<-done
	cancel()
	<-done
}

func forward(ctx context.Context, dst, src *websocket.Conn) {
	r := &messageReader{conn: src}
	for {
		typ, data, err := r.read(ctx)
		if status := websocket.CloseStatus(err); status != -1 {
			_ = dst.Close(status, "")
			return
		}
		if err != nil || dst.Write(ctx, typ, data) != nil {
			return
		}
	}
}
