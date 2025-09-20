package service

import (
	"context"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	seatv1 "github.com/yunmaoQu/kratos-12306/api/seat/v1"
	"github.com/google/uuid"
)

type SeatService struct {
	seatv1.UnimplementedSeatServiceServer
	log *log.Helper

	mu           sync.Mutex
	available    map[string]int // key: trip_id|seat_class
	reservations map[string]reservation
}

type reservation struct {
	tripID    string
	seatClass string
	count     int
	expireAt  time.Time
}

func NewSeatService(logger *log.Helper) *SeatService {
	return &SeatService{
		log:         logger,
		available:   map[string]int{"D1|2nd": 100, "D1|1st": 20},
		reservations: make(map[string]reservation),
	}
}

func key(tripID, seatClass string) string { return tripID + "|" + seatClass }

func (s *SeatService) Hold(ctx context.Context, in *seatv1.HoldRequest) (*seatv1.HoldReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(in.TripId, in.SeatClass)
	remain := s.available[k]
	if remain < int(in.Count) {
		return nil, seatv1.ErrorNoEnoughSeat("not enough seats: remain=%d need=%d", remain, in.Count)
	}
	s.available[k] = remain - int(in.Count)

	rid := uuid.NewString()
	exp := time.Now().Add(3 * time.Minute)
	s.reservations[rid] = reservation{
		tripID: in.TripId, seatClass: in.SeatClass, count: int(in.Count), expireAt: exp,
	}
	out := &seatv1.HoldReply{
		ReservationId: rid,
		ExpireAtUnix:  exp.Unix(),
	}
	for i := 0; i < int(in.Count); i++ {
		out.Assignments = append(out.Assignments, &seatv1.Assignment{CarriageNo: 1, SeatNo: "A"+string(rune('0'+i))})
	}
	return out, nil
}

func (s *SeatService) Release(ctx context.Context, in *seatv1.ReleaseRequest) (*seatv1.ReleaseReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.reservations[in.ReservationId]; ok {
		k := key(r.tripID, r.seatClass)
		s.available[k] += r.count
		delete(s.reservations, in.ReservationId)
		return &seatv1.ReleaseReply{Ok: true}, nil
	}
	return &seatv1.ReleaseReply{Ok: false}, nil
}