/*
Copyright 2019, 2020 the Velero contributors.

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

package backup

import (
	"fmt"

	snapshotv1beta1api "github.com/kubernetes-csi/external-snapshotter/v2/pkg/apis/volumesnapshot/v1beta1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"github.com/vmware-tanzu/velero-plugin-for-csi/internal/util"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	"github.com/vmware-tanzu/velero/pkg/util/boolptr"
)

// CSISnapshotter is a backup item action plugin for Velero.
type CSISnapshotter struct {
	Log logrus.FieldLogger
}

// AppliesTo returns information indicating that the CSISnapshotter action should be invoked to backup PVCs.
func (p *CSISnapshotter) AppliesTo() (velero.ResourceSelector, error) {
	p.Log.Info("CSISnapshotterAction AppliesTo")

	return velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
	}, nil
}

func setPVCAnnotationsAndLabels(pvc *corev1api.PersistentVolumeClaim, snapshotName, backupName string) {
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}

	pvc.Annotations[util.VolumeSnapshotLabel] = snapshotName
	pvc.Annotations[velerov1api.BackupNameLabel] = backupName

	if pvc.Labels == nil {
		pvc.Labels = make(map[string]string)
	}
	pvc.Labels[util.VolumeSnapshotLabel] = snapshotName
}

// Execute recognizes PVCs backed by volumes provisioned by CSI drivers with volumesnapshotting capability and creates snapshots of the
// underlying PVs by creating volumesnapshot CSI API objects that will trigger the CSI driver to perform the snapshot operation on the volume.
func (p *CSISnapshotter) Execute(item runtime.Unstructured, backup *velerov1api.Backup) (runtime.Unstructured, []velero.ResourceIdentifier, error) {
	p.Log.Info("Starting CSISnapshotterAction")

	// Do nothing if volume snapshots have not been requested in this backup
	if boolptr.IsSetToFalse(backup.Spec.SnapshotVolumes) {
		p.Log.Infof("Volume snapshotting not requested for backup %s/%s", backup.Namespace, backup.Name)
		return item, nil, nil
	}

	var pvc corev1api.PersistentVolumeClaim
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.UnstructuredContent(), &pvc); err != nil {
		return nil, nil, errors.WithStack(err)
	}

	client, snapshotClient, err := util.GetClients()
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	p.Log.Debugf("Fetching underlying PV for PVC %s", fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
	// Do nothing if this is not a CSI provisioned volume
	pv, err := util.GetPVForPVC(&pvc, client.CoreV1())
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if pv.Spec.PersistentVolumeSource.CSI == nil {
		p.Log.Infof("Skipping PVC %s/%s, associated PV %s is not a CSI volume", pvc.Namespace, pvc.Name, pv.Name)
		return item, nil, nil
	}

	// Do nothing if restic is used to backup this PV
	isResticUsed, err := util.IsPVCBackedUpByRestic(pvc.Namespace, pvc.Name, client.CoreV1())
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	if isResticUsed {
		p.Log.Infof("Skipping  PVC %s/%s, PV will be backed up using restic", pvc.Namespace, pvc.Name, pv.Name)
		return item, nil, nil
	}

	// no storage class: we don't know how to map to a VolumeSnapshotClass
	if pvc.Spec.StorageClassName == nil {
		return item, nil, errors.Errorf("Cannot snapshot PVC %s/%s, PVC has no storage class.", pvc.Namespace, pvc.Name)
	}

	p.Log.Infof("Fetching storage class for PV %s", *pvc.Spec.StorageClassName)
	storageClass, err := client.StorageV1().StorageClasses().Get(*pvc.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, errors.Wrap(err, "error getting storage class")
	}
	p.Log.Debugf("Fetching volumesnapshot class for %s", storageClass.Provisioner)
	snapshotClass, err := util.GetVolumeSnapshotClassForStorageClass(storageClass.Provisioner, snapshotClient.SnapshotV1beta1())
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get volumesnapshotclass for storageclass %s", storageClass.Name)
	}
	p.Log.Infof("volumesnapshot class=%s", snapshotClass.Name)

	// If deletetionPolicy is not Retain, then in the event of a disaster, the namespace is lost with the volumesnapshot object in it,
	// the underlying volumesnapshotcontent and the volume snapshot in the storage provider is also deleted.
	// In such a scenario, the backup objects will be useless as the snapshot handle itself will not be valid.
	if snapshotClass.DeletionPolicy != snapshotv1beta1api.VolumeSnapshotContentRetain {
		p.Log.Warnf("DeletionPolicy on VolumeSnapshotClass %s is not %s; Deletion of VolumeSnapshot objects will lead to deletion of snapshot in the storage provider.",
			snapshotClass.Name, snapshotv1beta1api.VolumeSnapshotContentRetain, snapshotClass.DeletionPolicy, snapshotv1beta1api.VolumeSnapshotContentRetain)
	}
	// Craft the snapshot object to be created
	snapshot := snapshotv1beta1api.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "velero-" + pvc.Name + "-",
			Namespace:    pvc.Namespace,
			Annotations: map[string]string{
				velerov1api.BackupNameLabel: backup.Name,
			},
		},
		Spec: snapshotv1beta1api.VolumeSnapshotSpec{
			Source: snapshotv1beta1api.VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvc.Name,
			},
			VolumeSnapshotClassName: &snapshotClass.Name,
		},
	}

	upd, err := snapshotClient.SnapshotV1beta1().VolumeSnapshots(pvc.Namespace).Create(&snapshot)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error creating volume snapshot")
	}
	p.Log.Infof("Created volumesnapshot %s", fmt.Sprintf("%s/%s", upd.Namespace, upd.Name))

	setPVCAnnotationsAndLabels(&pvc, upd.Name, backup.Name)

	p.Log.Info("Fetching volumesnapshotcontent for volumesnapshot")
	snapshotContent, err := util.GetVolumeSnapshotContentForVolumeSnapshot(upd, snapshotClient.SnapshotV1beta1(), p.Log)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	p.Log.Infof("Volumesnapshotcontent for volumesnapshot %s is %s", fmt.Sprintf("%s/%s", upd.Namespace, upd.Name), snapshotContent.Name)

	additionalItems := []velero.ResourceIdentifier{
		{
			GroupResource: kuberesource.VolumeSnapshotClasses,
			Name:          snapshotClass.Name,
		},
		{
			GroupResource: kuberesource.VolumeSnapshots,
			Namespace:     upd.Namespace,
			Name:          upd.Name,
		},
		{
			GroupResource: kuberesource.VolumeSnapshotContents,
			Name:          snapshotContent.Name,
		},
	}

	p.Log.Debug("Listing additional items to backup")
	for _, ai := range additionalItems {
		p.Log.Debugf("%s: %s", ai.GroupResource.String(), ai.Name)
	}

	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pvc)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	return &unstructured.Unstructured{Object: pvcMap}, additionalItems, nil
}
