package v1

// Minimal stubs to satisfy imports during development. Replace with generated code.
type CreateRequest struct{}
type CreateReply struct{}
type GetRequest struct{}
type GetReply struct{}
type MarkPaidRequest struct{ OrderNo, Channel, TxnId string }
type MarkPaidReply struct{ OrderNo, Status string }

type UnimplementedOrderServiceServer struct{}
