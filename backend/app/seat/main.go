package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-kratos/kratos/v2"
	klog "github.com/go-kratos/kratos/v2/log"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"

	seatv1 "github.com/yunmaoQu/kratos-12306/api/seat/v1"
	"github.com/yunmaoQu/kratos-12306/app/seat/internal/service"
)

func main() {
	logger := klog.NewStdLogger(os.Stdout)
	httpSrv := khttp.NewServer(
		khttp.Address(":8000"),
		khttp.Timeout(3*time.Second),
	)
	grpcSrv := kgrpc.NewServer(
		kgrpc.Address(":9000"),
		kgrpc.Timeout(3*time.Second),
	)

	svc := service.NewSeatService(klog.NewHelper(logger))
	seatv1.RegisterSeatServiceHTTPServer(httpSrv, svc)
	seatv1.RegisterSeatServiceServer(grpcSrv, svc)

	app := kratos.New(
		kratos.Name("seat"),
		kratos.Logger(logger),
		kratos.Server(httpSrv, grpcSrv),
	)
	if err := app.Run(); err != nil {
		fmt.Println(err)
	}
	_ = context.Background()
}
