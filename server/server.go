// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package server implements a grpc server to receive mount events
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/auth"
	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/config"
	"github.com/GoogleCloudPlatform/secrets-store-csi-driver-provider-gcp/csrmetrics"
	"github.com/googleapis/gax-go/v2"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/klog/v2"
	"sigs.k8s.io/secrets-store-csi-driver/provider/v1alpha1"
)

type Server struct {
	RuntimeVersion        string
	AuthClient            *auth.Client
	SecretClient          *secretmanager.Client
	RegionalSecretClients map[string]*secretmanager.Client
	SmOpts                []option.ClientOption
}

var _ v1alpha1.CSIDriverProviderServer = &Server{}

// Mount implements provider csi-provider method
func (s *Server) Mount(ctx context.Context, req *v1alpha1.MountRequest) (*v1alpha1.MountResponse, error) {
	p, err := strconv.ParseUint(req.GetPermission(), 10, 32)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Unable to parse permissions: %s", req.GetPermission()))

	}

	params := &config.MountParams{
		Attributes:  req.GetAttributes(),
		KubeSecrets: req.GetSecrets(),
		TargetPath:  req.GetTargetPath(),
		Permissions: os.FileMode(p),
	}

	cfg, err := config.Parse(params)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	ts, err := s.AuthClient.TokenSource(ctx, cfg)
	if err != nil {
		klog.ErrorS(err, "unable to obtain auth for mount", "pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})
		return nil, status.Error(codes.PermissionDenied, fmt.Sprintf("unable to obtain auth for mount: %v", err))
	}

	// Build a grpc credentials.PerRPCCredentials using
	// the grpc google.golang.org/grpc/credentials/oauth package, not to be
	// confused with the oauth2.TokenSource that it wraps.
	gts := oauth.TokenSource{TokenSource: ts}

	// Fetch the secrets from the secretmanager API based on the
	// SecretProviderClass configuration.
	return handleMountEvent(ctx, s.SecretClient, gts, cfg, s.RegionalSecretClients, s.SmOpts)
}

// Version implements provider csi-provider method
func (s *Server) Version(ctx context.Context, req *v1alpha1.VersionRequest) (*v1alpha1.VersionResponse, error) {
	return &v1alpha1.VersionResponse{
		Version:        "v1alpha1",
		RuntimeName:    "secrets-store-csi-driver-provider-gcp",
		RuntimeVersion: s.RuntimeVersion,
	}, nil
}

// handleMountEvent fetches the secrets from the secretmanager API and
// include them in the MountResponse based on the SecretProviderClass
// configuration.
func handleMountEvent(ctx context.Context, client *secretmanager.Client, creds credentials.PerRPCCredentials, cfg *config.MountConfig, regionalClients map[string]*secretmanager.Client, smOpts []option.ClientOption) (*v1alpha1.MountResponse, error) {
	results := make([]*secretmanagerpb.AccessSecretVersionResponse, len(cfg.Secrets))
	errs := make([]error, len(cfg.Secrets))

	// need to build a per-rpc call option based of the tokensource
	callAuth := gax.WithGRPCOptions(grpc.PerRPCCredentials(creds))

	// In parallel fetch all secrets needed for the mount
	wg := sync.WaitGroup{}
	for i, secret := range cfg.Secrets {
		loc, err := locationFromSecretResource(secret.ResourceName)
		if err != nil {
			errs[i] = err
			continue
		}

		if len(loc) > locationLengthLimit {
			errs[i] = fmt.Errorf("invalid location string, please check the location")
			continue
		}
		var secretClient *secretmanager.Client
		if loc == "" {
			secretClient = client
		} else {
			if _, ok := regionalClients[loc]; !ok {
				ep := option.WithEndpoint(fmt.Sprintf("secretmanager.%s.rep.googleapis.com:443", loc))
				regionalClient, err := secretmanager.NewClient(ctx, append(smOpts, ep)...)
				if err != nil {
					errs[i] = err
					continue
				}
				regionalClients[loc] = regionalClient
			}
			secretClient = regionalClients[loc]
		}
		wg.Add(1)
		i, secret := i, secret
		go func() {
			defer wg.Done()
			req := &secretmanagerpb.AccessSecretVersionRequest{
				Name: secret.ResourceName,
			}
			smMetricRecorder := csrmetrics.OutboundRPCStartRecorder("secretmanager_access_secret_version_requests")

			resp, err := secretClient.AccessSecretVersion(ctx, req, callAuth)
			if err != nil {
				if e, ok := status.FromError(err); ok {
					smMetricRecorder(csrmetrics.OutboundRPCStatus(e.Code().String()))
				}
			} else {
				smMetricRecorder(csrmetrics.OutboundRPCStatusOK)
			}
			results[i] = resp
			errs[i] = err
		}()
	}
	wg.Wait()

	// If any access failed, return a grpc status error that includes each
	// individual status error in the Details field.
	//
	// If there are any failures then there will be no changes to the
	// filesystem. Initial mount events will fail (preventing pod start) and
	// the secrets-store-csi-driver will emit pod events on rotation failures.
	// By erroring out on any failures we prevent partial rotations (i.e. the
	// username file was updated to a new value but the corresponding password
	// field was not).
	if err := buildErr(errs); err != nil {
		return nil, err
	}

	out := &v1alpha1.MountResponse{}

	// Add secrets to response.
	ovs := make([]*v1alpha1.ObjectVersion, len(cfg.Secrets))
	for i, secret := range cfg.Secrets {
		if cfg.Permissions > math.MaxInt32 {
			return nil, fmt.Errorf("invalid file permission %d", cfg.Permissions)
		}
		// #nosec G115 Checking limit
		mode := int32(cfg.Permissions)
		if secret.Mode != nil {
			mode = *secret.Mode
		}

		result := results[i]
		extractJSONKey := secret.ExtractJSONKey
		var content []byte

		// If extractJSONKey is null, then set the entire data
		if extractJSONKey == "" {
			content = result.Payload.Data
		} else {
			var data map[string]interface{}
			err := json.Unmarshal(result.Payload.Data, &data)
			if err != nil {
				return nil, fmt.Errorf("secret data not in JSON format")
			}

			value, ok := data[extractJSONKey]

			// If the key is not present, an error will be raised
			if !ok {
				return nil, fmt.Errorf("key %v does not exist at the secret path", extractJSONKey)
			} else {
				dataContent, ok := value.(string)

				// If there is a type conversion error
				if !ok {
					return nil, fmt.Errorf("wrong type for content, expected string")
				}
				content = []byte(dataContent)
			}
		}

		out.Files = append(out.Files, &v1alpha1.File{
			Path:     secret.PathString(),
			Mode:     mode,
			Contents: content,
		})
		klog.V(5).InfoS("added secret to response", "resource_name", secret.ResourceName, "file_name", secret.FileName, "pod", klog.ObjectRef{Namespace: cfg.PodInfo.Namespace, Name: cfg.PodInfo.Name})

		ovs[i] = &v1alpha1.ObjectVersion{
			Id:      secret.ResourceName,
			Version: result.GetName(),
		}
	}
	out.ObjectVersion = ovs

	return out, nil
}

// buildErr consolidates many errors into a single Status protobuf error message
// with each individual error included into the status Details any proto. The
// consolidated proto is converted to a general error.
func buildErr(errs []error) error {
	msgs := make([]string, 0, len(errs))
	hasErr := false
	s := &spb.Status{
		Code:    int32(codes.Internal),
		Details: make([]*anypb.Any, 0),
	}

	for i := range errs {
		if errs[i] == nil {
			continue
		}
		hasErr = true
		msgs = append(msgs, errs[i].Error())

		any, _ := anypb.New(status.Convert(errs[i]).Proto())
		s.Details = append(s.Details, any)
	}
	if !hasErr {
		return nil
	}
	s.Message = strings.Join(msgs, ",")
	return status.FromProto(s).Err()
}

// locationFromSecretResource returns location from the secret resource if the resource is in format "projects/<project_id>/locations/<location_id>/..."
// returns "" for global secret resource.
func locationFromSecretResource(resource string) (string, error) {
	globalSecretRegexp := regexp.MustCompile(globalSecretRegex)
	if m := globalSecretRegexp.FindStringSubmatch(resource); m != nil {
		return "", nil
	}
	regionalSecretRegexp := regexp.MustCompile(regionalSecretRegex)
	if m := regionalSecretRegexp.FindStringSubmatch(resource); m != nil {
		return m[2], nil
	}
	return "", status.Errorf(codes.InvalidArgument, "Invalid secret resource name: %s", resource)
}
