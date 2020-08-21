package storagecluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	ocsv1 "github.com/openshift/ocs-operator/pkg/apis/ocs/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	externalClusterDetailsSecret = "rook-ceph-external-cluster-details"
	externalClusterDetailsKey    = "external_cluster_details"
	cephFsStorageClassName       = "cephfs"
	cephRbdStorageClassName      = "ceph-rbd"
	cephRgwStorageClassName      = "ceph-rgw"
	externalCephRgwEndpointKey   = "endpoint"
)

const (
	rookCephOperatorConfigName = "rook-ceph-operator-config"
	rookEnableCephFSCSIKey     = "ROOK_CSI_ENABLE_CEPHFS"
)

var (
	// externalRgwEndpoint is the rgw endpoint as discovered in the Secret externalClusterDetailsSecret
	// It is used for independent mode only. It will be passed to the Noobaa CR as a label
	externalRgwEndpoint string
)

// ExternalResource containes a list of External Cluster Resources
type ExternalResource struct {
	Kind string            `json:"kind"`
	Data map[string]string `json:"data"`
	Name string            `json:"name"`
}

// setRookCSICephFS function enables or disables the 'ROOK_CSI_ENABLE_CEPHFS' key
func (r *ReconcileStorageCluster) setRookCSICephFS(
	enableDisableFlag bool, instance *ocsv1.StorageCluster, reqLogger logr.Logger) error {
	rookCephOperatorConfig := &corev1.ConfigMap{}
	err := r.client.Get(context.TODO(),
		types.NamespacedName{Name: rookCephOperatorConfigName, Namespace: instance.ObjectMeta.Namespace},
		rookCephOperatorConfig)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Unable to get '%s' config", rookCephOperatorConfigName))
		return err
	}
	enableDisableFlagStr := fmt.Sprintf("%v", enableDisableFlag)
	// if the current state of 'ROOK_CSI_ENABLE_CEPHFS' flag is same, just return
	if rookCephOperatorConfig.Data[rookEnableCephFSCSIKey] == enableDisableFlagStr {
		return nil
	}
	rookCephOperatorConfig.Data[rookEnableCephFSCSIKey] = enableDisableFlagStr
	return r.client.Update(context.TODO(), rookCephOperatorConfig)
}

// ensureExternalStorageClusterResources ensures that requested resources for the external cluster
// being created
func (r *ReconcileStorageCluster) ensureExternalStorageClusterResources(instance *ocsv1.StorageCluster, reqLogger logr.Logger) error {
	// check for the status boolean value accepted or not
	if instance.Status.ExternalSecretFound {
		return nil
	}
	found := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalClusterDetailsSecret,
			Namespace: instance.Namespace,
		},
	}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: found.Name, Namespace: found.Namespace}, found)
	if err != nil {
		return err
	}
	var data []ExternalResource
	err = json.Unmarshal(found.Data[externalClusterDetailsKey], &data)
	if err != nil {
		reqLogger.Error(err, "could not parse json blob")
		return err
	}
	err = r.createExternalStorageClusterResources(data, instance, reqLogger)
	if err != nil {
		reqLogger.Error(err, "could not create ExternalStorageClusterResource")
		return err
	}
	instance.Status.ExternalSecretFound = true
	return nil
}

// createExternalStorageClusterResources create the needed external cluster resources
func (r *ReconcileStorageCluster) createExternalStorageClusterResources(
	data []ExternalResource, instance *ocsv1.StorageCluster, reqLogger logr.Logger) error {
	ownerRef := metav1.OwnerReference{
		UID:        instance.UID,
		APIVersion: instance.APIVersion,
		Kind:       instance.Kind,
		Name:       instance.Name,
	}
	scs, err := r.newStorageClasses(instance)
	if err != nil {
		reqLogger.Error(err, "failed to create StorageClasses")
		return err
	}
	// this flag sets the 'ROOK_CSI_ENABLE_CEPHFS' flag
	enableRookCSICephFS := false
	// this stores only the StorageClasses specified in the Secret
	var availableSCs []*storagev1.StorageClass
	for _, d := range data {
		objectMeta := metav1.ObjectMeta{
			Name:            d.Name,
			Namespace:       instance.Namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		}
		objectKey := types.NamespacedName{Name: d.Name, Namespace: instance.Namespace}
		switch d.Kind {
		case "ConfigMap":
			cm := &corev1.ConfigMap{
				ObjectMeta: objectMeta,
				Data:       d.Data,
			}
			found := &corev1.ConfigMap{ObjectMeta: objectMeta}
			err := r.createExternalStorageClusterConfigMap(cm, found, reqLogger, objectKey)
			if err != nil {
				reqLogger.Error(err, "could not create ExternalStorageClusterConfigMap")
				return err
			}
		case "Secret":
			sec := &corev1.Secret{
				ObjectMeta: objectMeta,
				Data:       make(map[string][]byte),
			}
			for k, v := range d.Data {
				sec.Data[k] = []byte(v)
			}
			found := &corev1.Secret{ObjectMeta: objectMeta}
			err := r.createExternalStorageClusterSecret(sec, found, reqLogger, objectKey)
			if err != nil {
				reqLogger.Error(err, "could not create ExternalStorageClusterSecret")
				return err
			}
		case "StorageClass":
			var sc *storagev1.StorageClass
			if d.Name == cephFsStorageClassName {
				// 'sc' points to CephFS StorageClass
				sc = scs[0]
				enableRookCSICephFS = true
			} else if d.Name == cephRbdStorageClassName {
				// 'sc' points to RBD StorageClass
				sc = scs[1]
			} else if d.Name == cephRgwStorageClassName {
				// Set the external rgw endpoint variable for later use on the Noobaa CR (as a label)
				// Replace the colon with an underscore, otherwise the label will be invalid
				externalRgwEndpointReplaceColon := strings.Replace(d.Data[externalCephRgwEndpointKey], ":", "_", -1)
				externalRgwEndpoint = externalRgwEndpointReplaceColon

				// 'sc' points to OBC StorageClass
				sc = scs[2]
			}
			// now sc is pointing to appropriate StorageClass,
			// whose parameters have to be updated
			for k, v := range d.Data {
				sc.Parameters[k] = v
			}
			availableSCs = append(availableSCs, sc)
		}
	}
	// creating only the available storageClasses
	err = r.createStorageClasses(availableSCs, reqLogger)
	if err != nil {
		reqLogger.Error(err, "failed to create needed StorageClasses")
		return err
	}
	if err = r.setRookCSICephFS(enableRookCSICephFS, instance, reqLogger); err != nil {
		reqLogger.Error(err,
			fmt.Sprintf("failed to set '%s' to %v", rookEnableCephFSCSIKey, enableRookCSICephFS))
		return err
	}
	return nil
}

// createExternalStorageClusterConfigMap creates configmap for external cluster
func (r *ReconcileStorageCluster) createExternalStorageClusterConfigMap(cm *corev1.ConfigMap, found *corev1.ConfigMap, reqLogger logr.Logger, objectKey types.NamespacedName) error {
	err := r.client.Get(context.TODO(), objectKey, found)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info(fmt.Sprintf("creating configmap: %s", cm.Name))
			err = r.client.Create(context.TODO(), cm)
			if err != nil {
				reqLogger.Error(err, "creation of configmap failed")
				return err
			}
		} else {
			reqLogger.Error(err, "unable the get the configmap")
			return err
		}
	}
	return nil
}

// createExternalStorageClusterSecret creates secret for external cluster
func (r *ReconcileStorageCluster) createExternalStorageClusterSecret(sec *corev1.Secret, found *corev1.Secret, reqLogger logr.Logger, objectKey types.NamespacedName) error {
	err := r.client.Get(context.TODO(), objectKey, found)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info(fmt.Sprintf("creating secret: %s", sec.Name))
			err = r.client.Create(context.TODO(), sec)
			if err != nil {
				reqLogger.Error(err, "creation of secret failed")
				return err
			}
		} else {
			reqLogger.Error(err, "unable the get the secret")
			return err
		}
	}
	return nil
}
