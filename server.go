package server

import (
	"net"
	"sync"

	"github.com/bblfsh/server/runtime"

	"github.com/Sirupsen/logrus"
	"github.com/bblfsh/sdk/protocol"
	"google.golang.org/grpc"
	"gopkg.in/src-d/go-errors.v0"
)

var (
	ErrMissingDriver    = errors.NewKind("missing driver for language %s")
	ErrRuntime          = errors.NewKind("runtime failure")
	ErrAlreadyInstalled = errors.NewKind("driver already installed: %s (image reference: %s)")
)

// Server is a Babelfish server.
type Server struct {
	// Transport to use to fetch driver images. Defaults to "docker".
	// Useful transports:
	// - docker: uses Docker registries (docker.io by default).
	// - docker-daemon: gets images from a local Docker daemon.
	Transport string
	rt        *runtime.Runtime
	mu        sync.RWMutex
	drivers   map[string]Driver
}

func NewServer(r *runtime.Runtime) *Server {
	return &Server{
		rt:      r,
		drivers: make(map[string]Driver),
	}
}

func (s *Server) Serve(listener net.Listener) error {
	grpcServer := grpc.NewServer()

	logrus.Debug("registering gRPC service")
	protocol.RegisterProtocolServiceServer(
		grpcServer,
		protocol.NewProtocolServiceServer(),
	)

	protocol.DefaultParser = s

	logrus.Info("starting gRPC server")
	return grpcServer.Serve(listener)
}

func (s *Server) AddDriver(lang string, img string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.drivers[lang]
	if ok {
		return ErrAlreadyInstalled.New(lang, img)
	}

	image, err := runtime.NewDriverImage(img)
	if err != nil {
		return ErrRuntime.Wrap(err)
	}

	if err := s.rt.InstallDriver(image, false); err != nil {
		return ErrRuntime.Wrap(err)
	}

	dp, err := StartDriverPool(DefaultScalingPolicy(), DefaultPoolTimeout, func() (Driver, error) {
		return ExecDriver(s.rt, image)
	})
	if err != nil {
		return err
	}

	s.drivers[lang] = dp
	return nil
}

func (s *Server) Driver(lang string) (Driver, error) {
	s.mu.RLock()
	d, ok := s.drivers[lang]
	s.mu.RUnlock()
	if !ok {
		img := DefaultDriverImageReference(s.Transport, lang)
		err := s.AddDriver(lang, img)
		if err != nil && !ErrAlreadyInstalled.Is(err) {
			return nil, ErrMissingDriver.Wrap(err, lang)
		}

		s.mu.RLock()
		d = s.drivers[lang]
		s.mu.RUnlock()
	}

	return d, nil
}

func (s *Server) ParseUAST(req *protocol.ParseUASTRequest) *protocol.ParseUASTResponse {
	lang := req.Language
	if lang == "" {
		lang = GetLanguage(req.Filename, []byte(req.Content))
	}

	d, err := s.Driver(lang)
	if err != nil {
		return &protocol.ParseUASTResponse{
			Status: protocol.Fatal,
			Errors: []string{"error getting driver: " + err.Error()},
		}
	}

	return d.ParseUAST(req)
}

func (s *Server) Close() error {
	var err error
	for _, d := range s.drivers {
		if cerr := d.Close(); cerr != nil && err != nil {
			err = cerr
		}
	}

	return err
}
