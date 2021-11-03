// Copyright 2021 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// You may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/cosi-driver-minio/pkg/madmin"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	cosi "sigs.k8s.io/container-object-storage-interface-spec"

	"sigs.k8s.io/cosi-driver-minio/pkg/minio"
)

type ProvisionerServer struct {
	provisioner string
	mc          *minio.C
	adm         *madmin.AdminClient
}

// ProvisionerCreateBucket is an idempotent method for creating buckets
// It is expected to create the same bucket given a bucketName and protocol
// If the bucket already exists, then it MUST return codes.AlreadyExists
// Return values
//    nil -                   Bucket successfully created
//    codes.AlreadyExists -   Bucket already exists. No more retries
//    non-nil err -           Internal error                                [requeue'd with exponential backoff]
func (s *ProvisionerServer) ProvisionerCreateBucket(ctx context.Context,
	req *cosi.ProvisionerCreateBucketRequest) (*cosi.ProvisionerCreateBucketResponse, error) {

	protocol := req.GetProtocol()
	if protocol == nil {
		klog.ErrorS(errors.New("Invalid Argument"), "Protocol is nil")
		return nil, status.Error(codes.InvalidArgument, "Protocol is nil")
	}
	s3 := protocol.GetS3()
	if s3 == nil {
		klog.ErrorS(errors.New("Invalid Argument"), "S3 protocol is nil")
		return nil, status.Error(codes.InvalidArgument, "S3 Protocol is nil")
	}

	bucketName := req.GetName()
	klog.Infof("Call ProvisionerCreateBucket bucket:%s", bucketName)
	klog.V(3).InfoS("Create Bucket", "name", bucketName)

	options := minio.MakeBucketOptions{}

	// MinIO regions, unlike AWS s3 does not strictly require the
	// country-direction-index format. Therefore, no validation
	// is needed here
	options.Region = s3.Region

	// Support for the following two fields will be added
	// in the future using which bucket will be provisioned in a
	// particular region conforming to a particular signature version
	// However, as of now, these will be ignored

	// endpoint := s3.Endpoint
	// signatureVersion := s3.SignatureVersion

	// Since 'parameters' is not a typed construct
	// it is better to have predefined set of keys
	// to parse, rather than treating it as an opaque
	// set of keys and values.
	parameters := req.GetParameters()

	for k, v := range parameters {
		switch k {
		case minio.ObjectLocking:
			options.ObjectLocking = true
		default:
			klog.ErrorS(errors.New("Invalid Argument"), "parameter", k, "value", v)
			return nil, status.Error(codes.InvalidArgument, "invalid parameter")
		}
	}

	bucketID, err := s.mc.CreateBucket(ctx, bucketName, options)
	if err != nil {
		if err == minio.ErrBucketAlreadyExists {
			klog.InfoS("Bucket already exists", "name", bucketName)
			return &cosi.ProvisionerCreateBucketResponse{
				BucketId: bucketID,
			}, nil
		}
		klog.ErrorS(err, "Bucket creation failed")
		return nil, status.Error(codes.Internal, "Bucket creation failed")
	}

	return &cosi.ProvisionerCreateBucketResponse{
		BucketId: bucketID,
	}, nil
}

func (s *ProvisionerServer) ProvisionerDeleteBucket(ctx context.Context,
	req *cosi.ProvisionerDeleteBucketRequest) (*cosi.ProvisionerDeleteBucketResponse, error) {

	klog.Infof("Deleting bucket %q", req.GetBucketId())
	if err := s.mc.DeleteBucket(ctx, req.GetBucketId()); err != nil {
		klog.ErrorS(err, "failed to delete bucket %q", req.GetBucketId())
		return nil, status.Error(codes.Internal, "failed to delete bucket")
	}
	klog.Infof("Successfully deleted Bucket %q", req.GetBucketId())

	return &cosi.ProvisionerDeleteBucketResponse{}, nil

}

func (s *ProvisionerServer) ProvisionerGrantBucketAccess(ctx context.Context,
	req *cosi.ProvisionerGrantBucketAccessRequest) (*cosi.ProvisionerGrantBucketAccessResponse, error) {

	klog.Infof("Call ProvisionerGrantBucketAccess bucket:%s", req.GetBucketId())

	userName := req.GetAccountName()
	bucketName := req.GetBucketId()
	accessPolicy := req.GetAccessPolicy()

	statement := minio.NewPolicyStatement()
	err := json.Unmarshal([]byte(accessPolicy), statement)
	if err != nil {
		klog.Errorf("unmarshal policy failed err %s", err.Error())
		return nil, err
	}

	err = s.adm.AddUser(ctx, userName, s.mc.GetSecretKey())
	if err != nil {
		klog.Error("failed to create user", err)
		return nil, status.Error(codes.Internal, "User creation failed")
	}

	statement.WithSID(userName).
		ForPrincipals(userName).
		ForResources(bucketName).
		ForSubResources(bucketName)

	policy, err := s.mc.GetBucketPolicy(bucketName)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() != "NoSuchBucketPolicy" {
			return nil, status.Error(codes.Internal, "fetching policy failed")
		}
	}
	if policy == nil {
		policy = minio.NewBucketPolicy(*statement)
	} else {
		policy = policy.ModifyBucketPolicy(*statement)
	}
	err = s.mc.SettBucketPolicy(bucketName, policy)
	if err != nil {
		klog.Error("failed to set policy err:%s", err)
		return nil, status.Error(codes.Internal, "failed to set policy")
	}

	info, err := s.adm.GetUserInfo(ctx, userName)
	if err != nil {
		klog.Error("failed to get user", err)
		return nil, status.Error(codes.Internal, "User get failed")
	}

	return &cosi.ProvisionerGrantBucketAccessResponse{
		AccountId:               userName,
		CredentialsFileContents: fmt.Sprintf("[default]\naws_access_key %s\naws_secret_key %s", userName, info.SecretKey),
	}, nil
}

func (s *ProvisionerServer) ProvisionerRevokeBucketAccess(ctx context.Context,
	req *cosi.ProvisionerRevokeBucketAccessRequest) (*cosi.ProvisionerRevokeBucketAccessResponse, error) {

	klog.Infof("Deleting user %q", req.GetAccountId())
	userName := req.GetBucketId()
	if err := s.adm.RemoveUser(ctx, userName); err != nil {
		klog.Error("failed to Revoke Bucket Access")
		return nil, status.Error(codes.Internal, "failed to Revoke Bucket Access")
	}
	return &cosi.ProvisionerRevokeBucketAccessResponse{}, nil
}
