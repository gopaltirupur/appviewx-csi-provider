package provider

import (
	"context"
	"flag"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	apicorev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientConfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	pb "sigs.k8s.io/secrets-store-csi-driver/provider/v1alpha1"

	certorchestratorv1 "github.com/AppViewX/appviewx-csi-provider/cert-orchestrator/api/v1"
	v1 "github.com/AppViewX/appviewx-csi-provider/cert-orchestrator/api/v1"
	"github.com/AppViewX/appviewx-csi-provider/internal/config"
	"github.com/AppViewX/appviewx-csi-provider/internal/util"
)

var metricsAddr string
var enableLeaderElection bool
var probeAddr string
var mgr manager.Manager
var directClient client.Client

var (
	scheme            = runtime.NewScheme()
	setupLog          = ctrl.Log.WithName("setup")
	waitTimeInSeconds = 5
	maxWaitCount      = 120
	once              = sync.Once{}
	podCertCache      = map[string]bool{}
)

func setUpClient() {

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8090", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	var err error
	mgr, err = ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "394a58ec.certplus.appviewx",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.V(1).Error(err, "Error in creating Manager")
		return
	}
	setupLog.V(1).Info("Client Created")

	GroupVersion := schema.GroupVersion{Group: "cert-orchestrator.certplus.appviewx", Version: "v1"}
	metav1.AddToGroupVersion(scheme, GroupVersion)
	scheme.AddKnownTypes(GroupVersion, &certorchestratorv1.Cert{}, &certorchestratorv1.CertList{})

	err = apicorev1.AddToScheme(scheme)
	if err != nil {
		setupLog.V(1).Error(err, "Error in AddToScheme")
		return
	}

	directClient, err = client.New(clientConfig.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.V(1).Error(err, "Error in waitTillCertificatesAreCreatedAndRetrieveCertificateContents while getting the client : %v")
		return
	}
}

// provider implements the secrets-store-csi-driver provider interface
// and communicates with the Vault API.
type provider struct {
	logger hclog.Logger
	cache  map[cacheKey]*api.Secret

	// Allows mocking Kubernetes API for tests.
	k8sClient kubernetes.Interface
}

func NewProvider(logger hclog.Logger, k8sClient kubernetes.Interface) *provider {
	p := &provider{
		logger:    logger,
		cache:     make(map[cacheKey]*api.Secret),
		k8sClient: k8sClient,
	}

	return p
}

type cacheKey struct {
	secretPath string
	method     string
}

func getKey(podName, podNamespace string) string {
	return fmt.Sprintf("%s-%s", podName, podNamespace)
}

func createCertCRDs(
	ctx context.Context,
	l hclog.Logger,
	certSpecs []v1.CertSpec,
	podName, podNamespace string,
	uid types.UID,
) ([]v1.Cert, error) {

	l.Info(fmt.Sprintf("Started Creation of Cert for Pod : Name : %s : Namespace : %s",
		podName, podNamespace))
	currentClient := mgr.GetClient()

	certsCreated := []v1.Cert{}

	for _, currentCertSpec := range certSpecs {

		cert := certorchestratorv1.Cert{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    podNamespace,
				GenerateName: "appviewx-csi-provider" + "-" + podName + "-" + podNamespace + "-",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Pod",
						Name:       podName,
						UID:        uid,
					},
				},
			},
			Spec: currentCertSpec,
		}

		err := currentClient.Create(ctx, &cert, &client.CreateOptions{})
		if err != nil {
			l.Info(fmt.Sprintf("Error in create Certificate Pod : Name : %s : Namespace : %s : %v", podName, podNamespace, err))
			return nil, fmt.Errorf("Error in createCertCRDs while create Certificate Pod : Name : %s : Namespace : %s : %w",
				podName, podNamespace, err)
		}
		l.Info(fmt.Sprintf("Created certificate : Name : %s : Namespace : %s", cert.Name, cert.Namespace))
		certsCreated = append(certsCreated, cert)
	}
	l.Info("Cert Creation Success Pod : Name : %s : Namespace : %s", podName, podNamespace)
	return certsCreated, nil
}

func waitTillCertificatesAreCreatedAndRetrieveCertificateContents(
	ctx context.Context,
	l hclog.Logger,
	certs []v1.Cert) error {

	l.Info("Started waitTillCertificatesAreCreatedAndRetrieveCertificateContents")
	currentWaitCount := 0
	var allSecretsReady bool

	certsNameSpacedNames := []types.NamespacedName{}
	for _, currentCert := range certs {
		certsNameSpacedNames = append(certsNameSpacedNames, types.NamespacedName{
			Name:      currentCert.Name,
			Namespace: currentCert.Namespace,
		})
	}

	for currentWaitCount < maxWaitCount && !allSecretsReady {
		time.Sleep(time.Second * time.Duration(waitTimeInSeconds))
		allSecretsReady = true
		for i := 0; i < len(certs); i++ {
			cert := certs[i]

			certificateNamespacedName := types.NamespacedName{
				Name:      cert.Name,
				Namespace: cert.Namespace,
			}
			l.Info(fmt.Sprintf("waitTillCertificatesAreCreatedAndRetrieveCertificateContents - waiting for Certificate : "+
				"currentWaitCount : %d : i : %d certificateNamespacedName:%v", currentWaitCount, i, certificateNamespacedName))

			currentCert := v1.Cert{}
			err := directClient.Get(ctx, certificateNamespacedName, &currentCert)
			if err != nil {
				l.Info(fmt.Sprintf("OK - Error in Getting the Certificate : %v : %v", certificateNamespacedName, err))
				allSecretsReady = false
				break
			}
			if len(currentCert.Status.Certificate) <= 0 {
				allSecretsReady = false
				break
			} else {
				l.Info("Certificate Content available : %v", certificateNamespacedName)
				certs[i] = currentCert
			}
		}
		currentWaitCount++
	}

	if currentWaitCount == maxWaitCount {
		l.Info(fmt.Sprintf("waitTillCertificatesAreCreatedAndRetrieveCertificateContents - Failed : %v", certsNameSpacedNames))
		return fmt.Errorf("Error in waitTillCertificatesAreCreatedAndRetrieveCertificateContents - "+
			"currentWaitCount reached maxWaitCount : %v", certsNameSpacedNames)
	}
	l.Info(fmt.Sprintf("waitTillCertificatesAreCreatedAndRetrieveCertificateContents - Success : %v", certsNameSpacedNames))
	return nil
}

func getSecretContents(
	ctx context.Context,
	l hclog.Logger,
	secretNamespacedNames []types.NamespacedName,
) ([]map[string][]byte, error) {

	l.Info(fmt.Sprintf("Started getSecretContents : %v", secretNamespacedNames))

	output := []map[string][]byte{}

	for _, secretNamespacedName := range secretNamespacedNames {
		secretNamespacedName := types.NamespacedName{
			Name:      secretNamespacedName.Name,
			Namespace: secretNamespacedName.Namespace,
		}
		secret := apicorev1.Secret{}
		err := directClient.Get(ctx, secretNamespacedName, &secret)
		if err != nil {
			l.Error(fmt.Sprintf("Error in getting the Secret : Name : %s : Namespace : %s : %v",
				secretNamespacedName.Name, secretNamespacedName.Namespace, err))
			return nil, fmt.Errorf("Error in getSecretContents : %v : %w", secretNamespacedName, err)
		}
		if len(secret.Data) <= 0 {
			l.Error(fmt.Sprintf("Error in getSecretContents - Length of secret.Data is zero : %v", secretNamespacedName))
			return nil, fmt.Errorf("error in getSecretContents - Length of secret.Data is zero : %v", secretNamespacedName)
		}
		output = append(output, secret.Data)
	}
	l.Info(fmt.Sprintf("Finished getSecretContents : %v", secretNamespacedNames))
	return output, nil
}

// MountSecretsStoreObjectContent mounts content of the vault object to target path
func (p *provider) HandleMountRequest(
	ctx context.Context,
	cfg config.Config,
	flagsConfig config.FlagsConfig,
) (*pb.MountResponse, error) {

	podName := cfg.Parameters.PodInfo.Name
	podNameSpace := cfg.Parameters.PodInfo.Namespace
	uid := cfg.Parameters.PodInfo.UID

	p.logger.Info(fmt.Sprintf("Started HandleMountRequest : podName : %s : podNameSpace : %s", podName, podNameSpace))

	once.Do(setUpClient)

	secretNamespacedNames := []types.NamespacedName{}
	for _, certSpec := range cfg.Parameters.CertSpecs {
		secretNamespacedNames = append(secretNamespacedNames, types.NamespacedName{
			Name:      certSpec.SecretName,
			Namespace: podNameSpace,
		})
	}
	p.logger.Info(fmt.Sprintf("Length of secretNamespacedNames : %d : podName : %s : podNameSpace : %s",
		len(secretNamespacedNames), podName, podNameSpace))

	var err error
	var secretContents []map[string][]byte
	if secretContents, err = getSecretContents(ctx, p.logger, secretNamespacedNames); err != nil {
		p.logger.Info(fmt.Sprintf("Error in getSecretContents : %v", err))
		p.logger.Info(fmt.Sprintf("SecretContents are not available, Creating Certificate CRD : %v", secretNamespacedNames))

		if _, ok := podCertCache[getKey(podName, podNameSpace)]; ok {
			p.logger.Info(fmt.Sprintf("Cert Already Created for the POD : %s : PodNamespace : %s", podName, podNameSpace))
			return nil, nil
		} else {
			p.logger.Info(fmt.Sprintf("Cert Already Not Created for the POD : %s : PodNamespace : %s", podName, podNameSpace))
			podCertCache[getKey(podName, podNameSpace)] = true
		}

		createdCerts, err := createCertCRDs(ctx, p.logger, cfg.Parameters.CertSpecs, podName, podNameSpace, uid)
		if err != nil {
			p.logger.Error(fmt.Sprintf("Error in HandleMountRequest while createCertCRDs : %v", err))
			return nil, fmt.Errorf("Error in HandleMountRequest while createCertCRDs : %w", err)
		}

		err = waitTillCertificatesAreCreatedAndRetrieveCertificateContents(ctx, p.logger, createdCerts)
		if err != nil {
			p.logger.Error(fmt.Sprintf("Error in HandleMountRequest while waitTillCertificatesAreCreatedAndRetrieveCertificateContents : %v", err))
			return nil, fmt.Errorf("Error in HandleMountRequest while waitTillCertificatesAreCreatedAndRetrieveCertificateContent : %w", err)
		}

		//Try to get secrets after a second
		time.Sleep(time.Second)

		secretContents, err = getSecretContents(ctx, p.logger, secretNamespacedNames)
		if err != nil {
			p.logger.Error(fmt.Sprintf("Error in HandleMountRequest while getSecretContents : %v", err))
			return nil, fmt.Errorf("Error in HandleMountRequest while getSecretContents : %w", err)
		}

	} else {
		p.logger.Info(fmt.Sprintf("SecretContents are already available : %v", secretNamespacedNames))
	}

	var files []*pb.File
	var objectVersions []*pb.ObjectVersion

	for _, currentSecretContent := range secretContents {

		for k, v := range currentSecretContent {

			files = append(files, &pb.File{Path: k, Mode: int32(cfg.FilePermission), Contents: v})

			objectVersions = append(objectVersions, &pb.ObjectVersion{
				Id: k, Version: util.GetMD5Hash(v),
			})
		}
		//TODO: Need to handle for multiple secrets
		break
	}

	p.logger.Info("Finished HandleMountRequest")

	return &pb.MountResponse{
		Files:         files,
		ObjectVersion: objectVersions,
	}, nil
}
