package v1

type CreatePaymentRequest struct{ OrderNo, Channel string }
type CreatePaymentReply struct{ PayUrl string }
type CallbackRequest struct{ OrderNo, Channel, TxnId string }
type CallbackReply struct{ Ok bool }

type UnimplementedPaymentServiceServer struct{}
