package executor

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/hanfei1991/microcosom/model"
	"github.com/hanfei1991/microcosom/pb"
	"github.com/hanfei1991/microcosom/pkg/log"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type Server struct {
	cfg *Config

	srv *grpc.Server
	cli *MasterClient
	ID  model.ExecutorID

	lastHearbeatTime time.Time
}

func NewServer(cfg *Config) *Server {
	s := Server {
		cfg: cfg,
	}
	return &s
}

func (s *Server) SubmitSubJob(ctx context.Context, req *pb.SubmitSubJobRequest) (*pb.SubmitSubJobResponse, error) {
	return &pb.SubmitSubJobResponse{}, nil
}

func (s *Server) CancelSubJob(ctx context.Context, req *pb.CancelSubJobRequest) (*pb.CancelSubJobResponse, error) {
	return &pb.CancelSubJobResponse{}, nil
}

func (s *Server) Start(ctx context.Context) error {
	// Start grpc server

	rootLis, err := net.Listen("tcp", "127.0.0.1:10241")

	if err != nil {
		return err
	}

	log.L().Logger.Info("listen address", zap.String("addr", s.cfg.WorkerAddr))

	s.srv = grpc.NewServer()
	pb.RegisterExecutorServer(s.srv, s)

	grpcExitCh := make(chan struct{}, 1)

	go func() {
		err1 := s.srv.Serve(rootLis)
		if err1 != nil {
			log.L().Logger.Error("start grpc server failed", zap.Error(err))
		}
		grpcExitCh <- struct{}{}
	}()

	// Register myself
	s.cli, err = NewMasterClient(ctx, s.cfg)
	if err != nil {
		return err
	}
	log.L().Logger.Info("master client init successful")
	registerReq := &pb.RegisterExecutorRequest{
		Address: s.cfg.WorkerAddr,
		Capability: 100,
	}

	resp, err := s.cli.RegisterExecutor(ctx, registerReq)
	if err != nil {
		return err
	}
	s.ID = model.ExecutorID(resp.ExecutorId)
	log.L().Logger.Info("register successful", zap.Int32("id", int32(s.ID)))

	// Start Heartbeat
	ticker := time.NewTicker(time.Duration(s.cfg.KeepAliveInterval) * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <- grpcExitCh:
			return nil
		case t := <-ticker.C:
			req := &pb.HeartbeatRequest{
				ExecutorId: int32(s.ID),
				Status: int32(model.Running),
				Timestamp: uint64(t.Unix()),
				Ttl: uint64(s.cfg.KeepAliveTTL),
			}
			resp, err := s.cli.SendHeartbeat(ctx, req)
			if err != nil {
				log.L().Error("heartbeat meet error")
				if s.lastHearbeatTime.Add(time.Duration(s.cfg.KeepAliveTTL) * time.Millisecond).Before(time.Now()) {
					return err
				}
				continue
			}
			if resp.ErrMessage != "" {
				return errors.New(resp.ErrMessage)
			}
			log.L().Error("heartbeat success")
			s.lastHearbeatTime = t
		}
	}
}