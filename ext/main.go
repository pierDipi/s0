package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	applyconfigurationsautoscaling "k8s.io/client-go/applyconfigurations/autoscaling/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslister "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"knative.dev/pkg/signals"

	ext "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	health "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// DefaultResyncPeriod is the default duration that is used when no
	// resync period is associated with a controllers initialization context.
	DefaultResyncPeriod = 10 * time.Hour

	DefaultScaleDownCheckPeriod = 20 * time.Second

	DefaultDelay           = time.Minute // TODO: make this configurable
	DefaultMinReplicas     = int32(0)
	DefaultInitialReplicas = int32(1) // TODO: make this configurable
)

var (
	HardCodedTargets = []string{
		"default/backend",
	}
)

// server implements the ExternalProcessorServer interface.
type server struct {
	ext.UnimplementedExternalProcessorServer
	svcLister        corelisters.ServiceLister
	deploymentLister appslister.DeploymentLister
	k8s              kubernetes.Interface

	targetsLastRequest   map[string]time.Time
	targetsLastRequestMu sync.RWMutex
}

// Process is called for each HTTP request intercepted by Envoy.
func (s *server) Process(srv ext.ExternalProcessor_ProcessServer) error {
	log.Println("Starting to process a new stream")

	for {
		// Receive the next request
		req, err := srv.Recv()
		if err == io.EOF {
			log.Println("Stream closed by client")
			return nil
		}
		if err != nil {
			if errors.Is(err, status.Error(codes.Canceled, context.Canceled.Error())) {
				return nil
			}
			log.Printf("Error receiving request: %#v", err)
			return err
		}

		log.Println("Starting to process request")

		// Create response matching the request type
		var resp *ext.ProcessingResponse

		switch v := req.Request.(type) {
		case *ext.ProcessingRequest_RequestHeaders:
			log.Printf("Received request headers for %#v, context %#v", logHeaders(v.RequestHeaders.Headers.GetHeaders()), req.GetMetadataContext())
			host := GetAuthorityHeader(v.RequestHeaders.Headers.GetHeaders())
			go func() {
				if err := s.scaleUp(context.Background(), host); err != nil {
					log.Printf("Error scaling host %v: %v", host.RawValue, err)
				}
			}()
			resp = &ext.ProcessingResponse{
				Response: &ext.ProcessingResponse_RequestHeaders{
					RequestHeaders: &ext.HeadersResponse{
						Response: &ext.CommonResponse{
							Status: ext.CommonResponse_CONTINUE,
						},
					},
				},
			}
		case *ext.ProcessingRequest_ResponseHeaders:
			log.Println("Received response headers")
			log.Printf("Received response headers for %#v, context %#v", logHeaders(v.ResponseHeaders.Headers.GetHeaders()), req.GetMetadataContext())
			resp = &ext.ProcessingResponse{
				Response: &ext.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &ext.HeadersResponse{
						Response: &ext.CommonResponse{
							Status: ext.CommonResponse_CONTINUE,
						},
					},
				},
			}
		case *ext.ProcessingRequest_RequestBody:
			log.Println("Received request body")
			resp = &ext.ProcessingResponse{
				Response: &ext.ProcessingResponse_RequestBody{
					RequestBody: &ext.BodyResponse{
						Response: &ext.CommonResponse{
							Status: ext.CommonResponse_CONTINUE,
						},
					},
				},
			}
		case *ext.ProcessingRequest_ResponseBody:
			log.Println("Received response body")
			resp = &ext.ProcessingResponse{
				Response: &ext.ProcessingResponse_ResponseBody{
					ResponseBody: &ext.BodyResponse{
						Response: &ext.CommonResponse{
							Status: ext.CommonResponse_CONTINUE,
						},
					},
				},
			}
		default:
			log.Printf("Unknown request type: %T", req.Request)
			// This could be a different request type; fall back to a safe response
			resp = &ext.ProcessingResponse{
				Response: &ext.ProcessingResponse_ImmediateResponse{
					ImmediateResponse: &ext.ImmediateResponse{
						Status: &v3.HttpStatus{
							Code: 200,
						},
					},
				},
			}
		}

		log.Println("Sending response")

		// Send response
		if err := srv.Send(resp); err != nil {
			log.Printf("Error sending response: %v", err)
			return err
		}
	}
}

func GetAuthorityHeader(headers []*corev3.HeaderValue) *corev3.HeaderValue {
	for _, h := range headers {
		if h.Key == ":authority" {
			return h
		}
	}
	return nil
}

func (s *server) scaleUp(ctx context.Context, authority *corev3.HeaderValue) error {
	log.Println("Triggering scale-up operation asynchronously...", authority)

	parts := strings.SplitN(string(authority.RawValue), ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("error parsing authority: %s", authority)
	}

	name := strings.TrimSuffix(parts[0], "-s0")
	namespace := parts[1]

	log.Println("Scaling up service", namespace, name)

	svc, err := s.svcLister.Services(namespace).Get(name)
	if err != nil {
		return fmt.Errorf("error getting service %s/%s: %v", namespace, name, err)
	}
	if svc.Spec.Type == corev1.ServiceTypeClusterIP {
		selector := labels.SelectorFromSet(svc.Spec.Selector)

		deployments, err := s.deploymentLister.Deployments(svc.Namespace).List(selector)
		if err != nil {
			return fmt.Errorf("error getting deployments for service %s/%s with selector %v: %w", svc.Namespace, svc.Name, selector.String(), err)
		}

		for _, d := range deployments {

			s.targetsLastRequestMu.Lock()
			s.targetsLastRequest[fmt.Sprintf("%s/%s", d.Namespace, d.Name)] = time.Now()
			log.Printf("scaleUp.targetsLstRequest: %#v", s.targetsLastRequest)
			s.targetsLastRequestMu.Unlock()

			if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
				log.Println("Scaling up deployment", d.Namespace, d.Name)

				_, err = s.k8s.AppsV1().
					Deployments(d.Namespace).
					ApplyScale(ctx,
						d.Name,
						applyconfigurationsautoscaling.Scale().
							WithName(d.Name).
							WithNamespace(d.Namespace).
							WithUID(d.ObjectMeta.UID).
							WithResourceVersion(d.ObjectMeta.ResourceVersion).
							WithSpec(applyconfigurationsautoscaling.ScaleSpec().WithReplicas(DefaultInitialReplicas)),
						metav1.ApplyOptions{
							FieldManager: "s0-system",
							Force:        true,
						},
					)
				if err != nil {
					return fmt.Errorf("error applying scale for deployment %s/%s: %v", d.Namespace, d.Name, err)
				}
			}
		}
	}

	// Your scale-up logic here
	log.Println("Scale-up operation completed.")
	return nil
}

func (s *server) scaleDownLoop(ctx context.Context) {
	log.Println("Starting scale-down loop")

	ticker := time.NewTicker(DefaultScaleDownCheckPeriod)

	for {
		select {
		case <-ticker.C:
			var wg sync.WaitGroup

			log.Printf("Scale down loop started")
			s.targetsLastRequestMu.RLock()

			for key, t := range s.targetsLastRequest {
				if expire := t.Add(DefaultDelay); expire.Before(time.Now()) {
					wg.Add(1)
					go func() {
						// Important: Wrap the scale down in the locked phase to prevent other requests from interfering.
						s.targetsLastRequestMu.Lock()
						defer s.targetsLastRequestMu.Unlock()
						if err := s.scaleDown(ctx, key); err != nil {
							log.Println("Scale down error", err)
							return
						}
						delete(s.targetsLastRequest, key)
						wg.Done()
					}()
				} else {
					log.Printf("Skipping target for key %s: expire time %s", key, expire)
				}
			}
			s.targetsLastRequestMu.RUnlock()
			wg.Wait()
			log.Printf("Scale down loop completed")
		case <-ctx.Done():
			ticker.Stop()
			log.Println("Stopping scale-down loop")
			return
		}
	}
}

func (s *server) scaleDown(ctx context.Context, key string) error {
	log.Println("Scale-down", key)
	parts := strings.SplitN(key, string(types.Separator), 2)
	ns := parts[0]
	name := parts[1]

	d, err := s.deploymentLister.Deployments(ns).Get(name)
	if err != nil {
		return fmt.Errorf("error getting deployment %s/%s: %w", ns, name, err)
	}
	if d.Spec.Replicas != nil && *d.Spec.Replicas == DefaultMinReplicas {
		log.Println("Deployment is already scaled down", key, DefaultMinReplicas)
		return nil
	}

	_, err = s.k8s.AppsV1().Deployments(ns).
		ApplyScale(ctx,
			name,
			applyconfigurationsautoscaling.Scale().
				WithName(d.Name).
				WithNamespace(d.Namespace).
				WithUID(d.ObjectMeta.UID).
				WithResourceVersion(d.ObjectMeta.ResourceVersion).
				WithSpec(applyconfigurationsautoscaling.ScaleSpec().WithReplicas(DefaultMinReplicas)),
			metav1.ApplyOptions{
				FieldManager: "s0-system",
				Force:        true,
			},
		)
	if err != nil {
		return fmt.Errorf("failed to apply scale for deployment %s/%s: %v", ns, name, err)
	}
	return nil
}

func logHeaders(headers []*corev3.HeaderValue) string {
	sb := strings.Builder{}
	for _, h := range headers {
		sb.WriteString(h.Key)
		sb.WriteString("=")
		sb.WriteString(h.Value)
		sb.WriteString("=")
		sb.WriteString(string(h.RawValue))
		sb.WriteString(", ")
	}
	return sb.String()
}

func main() {
	ctx := signals.NewContext()

	// Listen on port 50051
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	time.Sleep(10 * time.Second) // Allow istio to start ...

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		overrides,
	).ClientConfig()
	if err != nil {
		log.Fatalf("failed to create client config: %v", err)
	}

	k8s := kubernetes.NewForConfigOrDie(config)
	factory := informers.NewSharedInformerFactoryWithOptions(
		k8s,
		DefaultResyncPeriod,
	)

	svcInformer := factory.Core().V1().Services()
	deploymentInformer := factory.Apps().V1().Deployments()

	// Create a gRPC server
	grpcServer := grpc.NewServer()

	// Register the ext_proc service
	srv := &server{
		svcLister:        svcInformer.Lister(),
		deploymentLister: deploymentInformer.Lister(),
		k8s:              k8s,
		// TODO init each target to "now" (meaning assume there was a request just before the server started to not scale down earlier than expected)
		targetsLastRequest:   make(map[string]time.Time, 64),
		targetsLastRequestMu: sync.RWMutex{},
	}
	// TODO remove hard coded targets
	srv.targetsLastRequestMu.Lock()
	for _, t := range HardCodedTargets {
		srv.targetsLastRequest[t] = time.Now()
	}
	srv.targetsLastRequestMu.Unlock()
	go srv.scaleDownLoop(ctx)

	ext.RegisterExternalProcessorServer(grpcServer, srv)
	health.RegisterHealthServer(grpcServer, &HealthServer{})

	// Register reflection service for debugging (optional)
	reflection.Register(grpcServer)

	// shutdown
	go func() {
		<-ctx.Done()
		log.Println("Gracefully shutting down...")
		grpcServer.GracefulStop()
		os.Exit(0)
	}()

	factory.Start(ctx.Done())
	for t, v := range factory.WaitForCacheSync(ctx.Done()) {
		if !v {
			log.Fatalf("failed to wait for cache sync for %#v: %v", t, err)
		}
	}

	log.Println("External processing server is listening on :50051")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}

type HealthServer struct{}

func (s *HealthServer) Check(ctx context.Context, in *health.HealthCheckRequest) (*health.HealthCheckResponse, error) {
	return &health.HealthCheckResponse{Status: health.HealthCheckResponse_SERVING}, nil
}

func (s *HealthServer) Watch(in *health.HealthCheckRequest, srv health.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "watch is not implemented")
}
