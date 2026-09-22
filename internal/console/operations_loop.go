package console

import (
	"context"
	"time"
)

// StartOperations runs the observer independently of browser refreshes. No cloud
// capacity mutation or game drain is performed by this monitoring loop.
func (d *DataSource) StartOperations(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.operations == nil || d.operationsDone != nil {
		return
	}
	background, cancel := context.WithCancel(ctx)
	d.operationsCancel = cancel
	d.operationsDone = make(chan struct{})
	go func() {
		defer close(d.operationsDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			value, err := d.Snapshot(background)
			if err == nil {
				if snapshot, err := apiObject(value); err == nil {
					d.operations.evaluate(background, snapshot)
				}
			}
			select {
			case <-background.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (d *DataSource) Close() {
	d.mu.Lock()
	cancel, done := d.operationsCancel, d.operationsDone
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if d.operations != nil && d.operations.closeFile != nil {
		d.operations.closeFile()
	}
}
