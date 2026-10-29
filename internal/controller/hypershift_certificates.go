package controller

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	envapi "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
var issuerGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"}

const hyperShiftAPISecret = "hypershift-api-tls"

func hyperShiftAPIHost(e *envapi.LLMWorkloadEnvironment, p HyperShiftProfile) string {
	id := sha256.Sum256([]byte(e.UID))
	return fmt.Sprintf("api-lwe-%x.%s", id[:6], p.BaseDomain)
}

// cert-manager owns all private key generation and renewal. The per-environment
// CA stays in the backing namespace and never needs access to HyperShift's CA.
func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftCertificate(ctx context.Context, e *envapi.LLMWorkloadEnvironment, ns string, p HyperShiftProfile) (bool, error) {
	bootstrap := hyperShiftObject(issuerGVK, ns, "hypershift-api-selfsigned")
	bootstrap.Object["spec"] = map[string]interface{}{"selfSigned": map[string]interface{}{}}
	if _, err := r.ensureHyperShiftManagedObject(ctx, e, bootstrap); err != nil {
		return false, fmt.Errorf("ensure cert-manager bootstrap Issuer: %w", err)
	}
	ca := hyperShiftCertificate(e, ns, "hypershift-api-ca", "hypershift-api-selfsigned")
	ca.Object["spec"].(map[string]interface{})["isCA"] = true
	ca.Object["spec"].(map[string]interface{})["commonName"] = fmt.Sprintf("LWE %s API CA", e.UID)
	ca.Object["spec"].(map[string]interface{})["duration"] = "87600h"
	ca.Object["spec"].(map[string]interface{})["renewBefore"] = "8760h"
	ca.Object["spec"].(map[string]interface{})["usages"] = []interface{}{"cert sign", "crl sign"}
	ca.Object["spec"].(map[string]interface{})["privateKey"] = map[string]interface{}{"algorithm": "RSA", "size": int64(2048), "rotationPolicy": "Never"}
	currentCA, err := r.ensureHyperShiftManagedObject(ctx, e, ca)
	if err != nil {
		return false, err
	}
	if !hyperShiftCondition(currentCA, "Ready") {
		return false, nil
	}
	if _, ready, err := r.hyperShiftCertificateSecret(ctx, e, ns, "hypershift-api-ca", "", true); err != nil || !ready {
		return false, err
	}
	issuer := hyperShiftObject(issuerGVK, ns, "hypershift-api-ca")
	issuer.Object["spec"] = map[string]interface{}{"ca": map[string]interface{}{"secretName": "hypershift-api-ca"}}
	if _, err := r.ensureHyperShiftManagedObject(ctx, e, issuer); err != nil {
		return false, err
	}
	cert := hyperShiftCertificate(e, ns, hyperShiftAPISecret, "hypershift-api-ca")
	cert.Object["spec"].(map[string]interface{})["dnsNames"] = []interface{}{hyperShiftAPIHost(e, p)}
	cert.Object["spec"].(map[string]interface{})["duration"] = "2160h"
	cert.Object["spec"].(map[string]interface{})["renewBefore"] = "720h"
	cert.Object["spec"].(map[string]interface{})["usages"] = []interface{}{"digital signature", "key encipherment", "server auth"}
	current, err := r.ensureHyperShiftManagedObject(ctx, e, cert)
	if err != nil {
		return false, err
	}
	if !hyperShiftCondition(current, "Ready") {
		return false, nil
	}
	_, ready, err := r.hyperShiftCertificateSecret(ctx, e, ns, hyperShiftAPISecret, hyperShiftAPIHost(e, p), false)
	return ready, err
}

func hyperShiftCertificate(e *envapi.LLMWorkloadEnvironment, ns, name, issuer string) *unstructured.Unstructured {
	cert := hyperShiftObject(certificateGVK, ns, name)
	cert.Object["spec"] = map[string]interface{}{
		"secretName":     name,
		"issuerRef":      map[string]interface{}{"name": issuer, "kind": "Issuer", "group": "cert-manager.io"},
		"secretTemplate": map[string]interface{}{"annotations": stringMap(environmentOwnershipAnnotations(e))},
		"privateKey":     map[string]interface{}{"algorithm": "RSA", "size": int64(2048), "rotationPolicy": "Always"},
	}
	return cert
}

func (r *LLMWorkloadEnvironmentReconciler) ensureHyperShiftManagedObject(ctx context.Context, e *envapi.LLMWorkloadEnvironment, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired.SetAnnotations(environmentOwnershipAnnotations(e))
	actual := hyperShiftObject(desired.GroupVersionKind(), desired.GetNamespace(), desired.GetName())
	err := r.hyperShiftReader().Get(ctx, client.ObjectKeyFromObject(actual), actual)
	if apierrors.IsNotFound(err) {
		// cert-manager can populate an existing target Secret. Refuse a foreign
		// Secret before creating a Certificate that could overwrite it.
		if desired.GroupVersionKind() == certificateGVK {
			secretName, _, _ := unstructured.NestedString(desired.Object, "spec", "secretName")
			secret := &corev1.Secret{}
			if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: secretName}, secret); err == nil {
				if !environmentOwns(secret, e) || secret.Annotations["cert-manager.io/certificate-name"] != desired.GetName() {
					return nil, fmt.Errorf("refusing to issue a certificate into foreign Secret %s", secretName)
				}
			} else if !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if !environmentOwns(actual, e) || actual.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("%s %s ownership is invalid or deletion is in progress", actual.GetKind(), actual.GetName())
	}
	// Strategy markers such as selfSigned:{} must remain present. The general
	// subset comparison tolerates omitted empty admission-defaulted objects.
	wantedSpec, _ := desired.Object["spec"].(map[string]interface{})
	actualSpec, _ := actual.Object["spec"].(map[string]interface{})
	for key := range wantedSpec {
		if _, exists := actualSpec[key]; !exists {
			return nil, fmt.Errorf("%s %s configuration drifted", actual.GetKind(), actual.GetName())
		}
	}
	if !hyperShiftSubset(desired.Object["spec"], actual.Object["spec"]) {
		return nil, fmt.Errorf("%s %s configuration drifted", actual.GetKind(), actual.GetName())
	}
	return actual, nil
}

func (r *LLMWorkloadEnvironmentReconciler) hyperShiftCertificateSecret(ctx context.Context, e *envapi.LLMWorkloadEnvironment, ns, name, host string, isCA bool) (*corev1.Secret, bool, error) {
	secret := &corev1.Secret{}
	if err := r.hyperShiftReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !environmentOwns(secret, e) || secret.Type != corev1.SecretTypeTLS || secret.Annotations["cert-manager.io/certificate-name"] != name {
		return nil, false, fmt.Errorf("cert-manager TLS Secret %s ownership or type is invalid", name)
	}
	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil || len(pair.Certificate) == 0 {
		return nil, false, fmt.Errorf("cert-manager TLS Secret %s has no valid key pair", name)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
		return nil, false, fmt.Errorf("cert-manager TLS Secret %s certificate is not currently valid", name)
	}
	if isCA {
		if !cert.IsCA {
			return nil, false, fmt.Errorf("cert-manager root certificate is not a CA")
		}
	} else {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
			return nil, false, fmt.Errorf("cert-manager TLS Secret is missing its CA bundle")
		}
		intermediates := x509.NewCertPool()
		rest := secret.Data[corev1.TLSCertKey]
		for {
			block, remaining := pem.Decode(rest)
			if block == nil {
				break
			}
			rest = remaining
			if intermediate, err := x509.ParseCertificate(block.Bytes); err == nil {
				intermediates.AddCert(intermediate)
			}
		}
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: intermediates}); err != nil {
			return nil, false, fmt.Errorf("cert-manager TLS Secret does not verify for the API hostname: %w", err)
		}
	}
	return secret, true, nil
}
