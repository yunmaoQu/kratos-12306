package v1
type HoldRequest struct{ TripId, SeatClass string; Count int32 }
type HoldReply struct{ ReservationId string; ExpireAtUnix int64; Assignments []*Assignment }
type ReleaseRequest struct{ ReservationId string }
type ReleaseReply struct{ Ok bool }
type Assignment struct{ CarriageNo int32; SeatNo string }

type UnimplementedSeatServiceServer struct{}
