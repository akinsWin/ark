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

package gcp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	uuid "github.com/satori/go.uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v0.beta"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/heptio/ark/pkg/cloudprovider"
)

type blockStorageAdapter struct {
	gce     *compute.Service
	project string
	zone    string
}

var _ cloudprovider.BlockStorageAdapter = &blockStorageAdapter{}

func NewBlockStorageAdapter(project, zone string) (cloudprovider.BlockStorageAdapter, error) {
	if project == "" {
		return nil, errors.New("missing project in gcp configuration in config file")
	}
	if zone == "" {
		return nil, errors.New("missing zone in gcp configuration in config file")
	}

	client, err := google.DefaultClient(oauth2.NoContext, compute.ComputeScope)
	if err != nil {
		return nil, err
	}

	gce, err := compute.New(client)
	if err != nil {
		return nil, err
	}

	// validate project & zone
	res, err := gce.Zones.Get(project, zone).Do()
	if err != nil {
		return nil, err
	}

	if res == nil {
		return nil, fmt.Errorf("zone %q not found for project %q", project, zone)
	}

	return &blockStorageAdapter{
		gce:     gce,
		project: project,
		zone:    zone,
	}, nil
}

func (op *blockStorageAdapter) CreateVolumeFromSnapshot(snapshotID string, volumeType string, iops *int64) (volumeID string, err error) {
	res, err := op.gce.Snapshots.Get(op.project, snapshotID).Do()
	if err != nil {
		return "", err
	}

	disk := &compute.Disk{
		Name:           "restore-" + uuid.NewV4().String(),
		SourceSnapshot: res.SelfLink,
		Type:           volumeType,
	}

	if _, err = op.gce.Disks.Insert(op.project, op.zone, disk).Do(); err != nil {
		return "", err
	}

	return disk.Name, nil
}

func (op *blockStorageAdapter) GetVolumeInfo(volumeID string) (string, *int64, error) {
	res, err := op.gce.Disks.Get(op.project, op.zone, volumeID).Do()
	if err != nil {
		return "", nil, err
	}

	return res.Type, nil, nil
}

func (op *blockStorageAdapter) IsVolumeReady(volumeID string) (ready bool, err error) {
	disk, err := op.gce.Disks.Get(op.project, op.zone, volumeID).Do()
	if err != nil {
		return false, err
	}

	// TODO can we consider a disk ready while it's in the RESTORING state?
	return disk.Status == "READY", nil
}

func (op *blockStorageAdapter) ListSnapshots(tagFilters map[string]string) ([]string, error) {
	useParentheses := len(tagFilters) > 1
	subFilters := make([]string, 0, len(tagFilters))

	for k, v := range tagFilters {
		fs := k + " eq " + v
		if useParentheses {
			fs = "(" + fs + ")"
		}
		subFilters = append(subFilters, fs)
	}

	filter := strings.Join(subFilters, " ")

	res, err := op.gce.Snapshots.List(op.project).Filter(filter).Do()
	if err != nil {
		return nil, err
	}

	ret := make([]string, 0, len(res.Items))
	for _, snap := range res.Items {
		ret = append(ret, snap.Name)
	}

	return ret, nil
}

func (op *blockStorageAdapter) CreateSnapshot(volumeID string, tags map[string]string) (string, error) {
	// snapshot names must adhere to RFC1035 and be 1-63 characters
	// long
	var snapshotName string
	suffix := "-" + uuid.NewV4().String()

	if len(volumeID) <= (63 - len(suffix)) {
		snapshotName = volumeID + suffix
	} else {
		snapshotName = volumeID[0:63-len(suffix)] + suffix
	}

	gceSnap := compute.Snapshot{
		Name: snapshotName,
	}

	_, err := op.gce.Disks.CreateSnapshot(op.project, op.zone, volumeID, &gceSnap).Do()
	if err != nil {
		return "", err
	}

	// the snapshot is not immediately available after creation for putting labels
	// on it. poll for a period of time.
	if pollErr := wait.Poll(1*time.Second, 30*time.Second, func() (bool, error) {
		if res, err := op.gce.Snapshots.Get(op.project, gceSnap.Name).Do(); err == nil {
			gceSnap = *res
			return true, nil
		}
		return false, nil
	}); pollErr != nil {
		return "", err
	}

	labels := &compute.GlobalSetLabelsRequest{
		Labels:           tags,
		LabelFingerprint: gceSnap.LabelFingerprint,
	}

	_, err = op.gce.Snapshots.SetLabels(op.project, gceSnap.Name, labels).Do()
	if err != nil {
		return "", err
	}

	return gceSnap.Name, nil
}

func (op *blockStorageAdapter) DeleteSnapshot(snapshotID string) error {
	_, err := op.gce.Snapshots.Delete(op.project, snapshotID).Do()

	return err
}
