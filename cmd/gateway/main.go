package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ginlogger "github.com/gin-contrib/logger"
	"github.com/gin-gonic/gin"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/kelseyhightower/envconfig"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/kzmake/greeter/api/greeter/v1"
)

type Env struct {
	Address string `default:"0.0.0.0:8080"`
	Service struct {
		Address string `default:"greeter.default.svc.cluster.local:50051"`
	}
	MTLS bool `default:"true"`
}

const (
	prefix  = "GATEWAY"
	crtFile = "certs/client.gateway.crt"
	keyFile = "certs/client.gateway.key"
	caFile  = "certs/ca.crt"
)

var (
	env   Env
	creds credentials.TransportCredentials
)

func init() {
	if err := envconfig.Process(prefix, &env); err != nil {
		panic(err)
	}

	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	log.Debug().Msgf("%+v", env)

	if env.MTLS {
		var err error
		creds, err = loadCreds()
		if err != nil {
			log.Panic().Msgf("%+v", err)
		}
	}
}

func loadCreds() (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(crtFile, keyFile)
	if err != nil {
		return nil, xerrors.Errorf("failed to load %s or %s: %w", crtFile, keyFile, err)
	}

	ca, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, xerrors.Errorf("failed to load %s: %w", caFile, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(ca) {
		return nil, xerrors.Errorf("failed to append certificates")
	}

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      cp,
	}), nil
}

func newServer(ctx context.Context) (*http.Server, error) {
	h := runtime.NewServeMux()
	opts := []grpc.DialOption{}
	if env.MTLS {
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	if err := pb.RegisterGreeterHandlerFromEndpoint(ctx, h, env.Service.Address, opts); err != nil {
		return nil, xerrors.Errorf("Failed to register handler: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(ginlogger.SetLogger(
		ginlogger.WithLogger(func(c *gin.Context, _ io.Writer, latency time.Duration) zerolog.Logger {
			return log.Logger.With().
				Timestamp().
				Int("status", c.Writer.Status()).
				Str("method", c.Request.Method).
				Str("path", c.Request.URL.Path).
				Str("ip", c.ClientIP()).
				Dur("latency", latency).
				Str("user_agent", c.Request.UserAgent()).
				Logger()
		}),
		ginlogger.WithSkipPath([]string{"*"}),
	))
	r.Use(gin.Recovery())

	r.Any("/*any", gin.WrapH(h))

	return &http.Server{Addr: env.Address, Handler: r}, nil
}

func run() error {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	gatewayS, err := newServer(ctx)
	if err != nil {
		log.Fatal().Msgf("Failed to build gateway server: %v", err)
	}
	g.Go(func() error { return gatewayS.ListenAndServe() })

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	select {
	case <-quit:
		break
	case <-ctx.Done():
		break
	}

	cancel()

	log.Info().Msg("Shutting down server...")

	ctx, timeout := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeout()

	if err := gatewayS.Shutdown(ctx); err != nil {
		return xerrors.Errorf("failed to shutdown: %w", err)
	}

	log.Info().Msgf("Server exiting")

	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal().Msgf("Failed to run server: %v", err)
	}
}
