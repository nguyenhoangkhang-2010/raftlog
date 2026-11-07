// Package client is the Go client for a raftlog cluster.
//
// Give it the addresses of all the nodes. It finds the leader, sends writes
// there, skips nodes that are down and waits out elections, so a program does
// not have to know which node is the leader or handle a failover itself.
//
//	c, err := client.New([]string{"10.0.0.1:9092", "10.0.0.2:9092", "10.0.0.3:9092"})
//	if err != nil {
//		return err
//	}
//	defer c.Close()
//
//	if err := c.CreateTopic(ctx, "orders", 2); err != nil && !errors.Is(err, client.ErrAlreadyExists) {
//		return err
//	}
//	res, err := c.Produce(ctx, "orders", []client.Message{{Key: []byte("user-7"), Value: []byte("paid")}})
//	// res.Partition and res.Offset say where the message is.
//
//	records, err := c.Fetch(ctx, "orders", res.Partition, res.Offset, 100)
//
// To wait for the next message instead of asking over and over, pass
// client.WaitUpTo; the context must outlive the wait:
//
//	records, err := c.Fetch(ctx, "orders", p, next, 100, client.WaitUpTo(20*time.Second))
//
// Every request takes a context. Without a deadline a request waits for as long
// as it takes to find a leader; give each request one.
//
// Delivery is at-least-once: when a request fails after it was sent, the client
// retries, and a write can end up stored twice. Design consumers so that a
// repeated message is harmless, for instance by giving messages an identifier
// and ignoring one that was already seen.
//
// Router is the lower-level type: the same routing behind the raw protobuf API.
package client
