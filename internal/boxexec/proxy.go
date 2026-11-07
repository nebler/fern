package boxexec

import (
	"context"
	"sync"

	"github.com/coder/websocket"
)

// Proxy copies messages between client and box, both ways, until either side
// ends or ctx does, and passes a close status on to the other side. When it
// ends for any other reason it closes both with a close message: to a box, a
// connection that breaks without one means the daemon has gone and a session's
// command should wait for it (session.go). hold says when that is so, which
// leaves both without one. It may be nil.
func Proxy(ctx context.Context, client, box *websocket.Conn, hold func() bool) {
	end := func() {
		if hold != nil && hold() {
			_ = client.CloseNow()
			_ = box.CloseNow()
			return
		}
		var both sync.WaitGroup
		both.Go(func() { _ = client.Close(websocket.StatusGoingAway, "") })
		both.Go(func() { _ = box.Close(websocket.StatusGoingAway, "") })
		both.Wait()
	}
	stop := context.AfterFunc(ctx, end)
	defer stop()
	// Reads don't watch ctx: they'd close their connection with no message.
	rctx := context.WithoutCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { forward(rctx, client, box); done <- struct{}{} }()
	go func() { forward(rctx, box, client); done <- struct{}{} }()
	<-done
	end()
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
