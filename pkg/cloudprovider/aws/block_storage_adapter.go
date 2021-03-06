/*
Copyright 2017 Heptio Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/heptio/ark/pkg/cloudprovider"
)

var _ cloudprovider.BlockStorageAdapter = &blockStorageAdapter{}

type blockStorageAdapter struct {
	ec2 *ec2.EC2
	az  string
}

func getSession(config *aws.Config) (*session.Session, error) {
	sess, err := session.NewSession(config)
	if err != nil {
		return nil, err
	}

	if _, err := sess.Config.Credentials.Get(); err != nil {
		return nil, err
	}

	return sess, nil
}

func NewBlockStorageAdapter(region, availabilityZone string) (cloudprovider.BlockStorageAdapter, error) {
	if region == "" {
		return nil, errors.New("missing region in aws configuration in config file")
	}
	if availabilityZone == "" {
		return nil, errors.New("missing availabilityZone in aws configuration in config file")
	}

	awsConfig := aws.NewConfig().WithRegion(region)

	sess, err := getSession(awsConfig)
	if err != nil {
		return nil, err
	}

	// validate the availabilityZone
	var (
		ec2Client = ec2.New(sess)
		azReq     = &ec2.DescribeAvailabilityZonesInput{ZoneNames: []*string{&availabilityZone}}
	)
	res, err := ec2Client.DescribeAvailabilityZones(azReq)
	if err != nil {
		return nil, err
	}
	if len(res.AvailabilityZones) == 0 {
		return nil, fmt.Errorf("availability zone %q not found", availabilityZone)
	}

	return &blockStorageAdapter{
		ec2: ec2Client,
		az:  availabilityZone,
	}, nil
}

// iopsVolumeTypes is a set of AWS EBS volume types for which IOPS should
// be captured during snapshot and provided when creating a new volume
// from snapshot.
var iopsVolumeTypes = sets.NewString("io1")

func (op *blockStorageAdapter) CreateVolumeFromSnapshot(snapshotID, volumeType string, iops *int64) (volumeID string, err error) {
	req := &ec2.CreateVolumeInput{
		SnapshotId:       &snapshotID,
		AvailabilityZone: &op.az,
		VolumeType:       &volumeType,
	}

	if iopsVolumeTypes.Has(volumeType) && iops != nil {
		req.Iops = iops
	}

	res, err := op.ec2.CreateVolume(req)
	if err != nil {
		return "", err
	}

	return *res.VolumeId, nil
}

func (op *blockStorageAdapter) GetVolumeInfo(volumeID string) (string, *int64, error) {
	req := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{&volumeID},
	}

	res, err := op.ec2.DescribeVolumes(req)
	if err != nil {
		return "", nil, err
	}

	if len(res.Volumes) != 1 {
		return "", nil, fmt.Errorf("Expected one volume from DescribeVolumes for volume ID %v, got %v", volumeID, len(res.Volumes))
	}

	vol := res.Volumes[0]

	var (
		volumeType string
		iops       *int64
	)

	if vol.VolumeType != nil {
		volumeType = *vol.VolumeType
	}

	if iopsVolumeTypes.Has(volumeType) && vol.Iops != nil {
		iops = vol.Iops
	}

	return volumeType, iops, nil
}

func (op *blockStorageAdapter) IsVolumeReady(volumeID string) (ready bool, err error) {
	req := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{&volumeID},
	}

	res, err := op.ec2.DescribeVolumes(req)
	if err != nil {
		return false, err
	}
	if len(res.Volumes) != 1 {
		return false, fmt.Errorf("Expected one volume from DescribeVolumes for volume ID %v, got %v", volumeID, len(res.Volumes))
	}

	return *res.Volumes[0].State == ec2.VolumeStateAvailable, nil
}

func (op *blockStorageAdapter) ListSnapshots(tagFilters map[string]string) ([]string, error) {
	req := &ec2.DescribeSnapshotsInput{}

	for k, v := range tagFilters {
		filter := &ec2.Filter{}
		filter.SetName(k)
		filter.SetValues([]*string{&v})

		req.Filters = append(req.Filters, filter)
	}

	res, err := op.ec2.DescribeSnapshots(req)
	if err != nil {
		return nil, err
	}

	var ret []string

	for _, snapshot := range res.Snapshots {
		ret = append(ret, *snapshot.SnapshotId)
	}

	return ret, nil
}

func (op *blockStorageAdapter) CreateSnapshot(volumeID string, tags map[string]string) (string, error) {
	req := &ec2.CreateSnapshotInput{
		VolumeId: &volumeID,
	}

	res, err := op.ec2.CreateSnapshot(req)
	if err != nil {
		return "", err
	}

	tagsReq := &ec2.CreateTagsInput{}
	tagsReq.SetResources([]*string{res.SnapshotId})

	ec2Tags := make([]*ec2.Tag, 0, len(tags))

	for k, v := range tags {
		key := k
		val := v

		tag := &ec2.Tag{Key: &key, Value: &val}
		ec2Tags = append(ec2Tags, tag)
	}

	tagsReq.SetTags(ec2Tags)

	_, err = op.ec2.CreateTags(tagsReq)

	return *res.SnapshotId, err
}

func (op *blockStorageAdapter) DeleteSnapshot(snapshotID string) error {
	req := &ec2.DeleteSnapshotInput{
		SnapshotId: &snapshotID,
	}

	_, err := op.ec2.DeleteSnapshot(req)

	return err
}
