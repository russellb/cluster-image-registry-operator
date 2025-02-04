package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2019-04-01/storage"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	operatorapiv1 "github.com/openshift/api/operator/v1"
	imageregistryv1 "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1"
	regopclient "github.com/openshift/cluster-image-registry-operator/pkg/client"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage/util"
)

const (
	blobFormatString = `https://%s.blob.core.windows.net`

	storageExistsReasonNotConfigured     = "StorageNotConfigured"
	storageExistsReasonConfigError       = "ConfigError"
	storageExistsReasonUserManaged       = "UserManaged"
	storageExistsReasonAzureError        = "AzureError"
	storageExistsReasonContainerNotFound = "ContainerNotFound"
	storageExistsReasonContainerExists   = "ContainerExists"
	storageExistsReasonContainerDeleted  = "ContainerDeleted"
	storageExistsReasonAccountDeleted    = "AccountDeleted"
)

var (
	// storageAccountInvalidCharRe is a regular expression for characters that
	// cannot be used in Azure storage accounts names (i.e. that are not
	// numbers nor lower-case letters) and that are not upper-case letters. If
	// you use this regular expression to filter invalid characters, you also
	// need to strings.ToLower to get a valid storage account name or an empty
	// string.
	storageAccountInvalidCharRe = regexp.MustCompile(`[^0-9A-Za-z]`)
)

type Azure struct {
	// IPI
	SubscriptionID string
	ClientID       string
	ClientSecret   string
	TenantID       string
	ResourceGroup  string
	Region         string

	// UPI
	AccountKey string
}

type errNameNotAvailable struct {
	AccountName string
	Message     string
}

func (e *errNameNotAvailable) Error() string {
	return fmt.Sprintf("storage account name %s is not available: %s", e.AccountName, e.Message)
}

type errDoesNotExist struct {
	Err error
}

func (e *errDoesNotExist) Error() string {
	return e.Err.Error()
}

func getAzureConfigFromCloudSecret(creds *corev1.Secret) (*Azure, error) {
	cfg := &Azure{}
	cfg.SubscriptionID = string(creds.Data["azure_subscription_id"])
	cfg.ClientID = string(creds.Data["azure_client_id"])
	cfg.ClientSecret = string(creds.Data["azure_client_secret"])
	cfg.TenantID = string(creds.Data["azure_tenant_id"])
	cfg.ResourceGroup = string(creds.Data["azure_resourcegroup"])
	cfg.Region = string(creds.Data["azure_region"])
	return cfg, nil
}

func getAzureConfigFromUserSecret(sec *corev1.Secret) (*Azure, error) {
	cfg := &Azure{}
	var err error

	cfg.AccountKey, err = util.GetValueFromSecret(sec, "REGISTRY_STORAGE_AZURE_ACCOUNTKEY")
	if err != nil {
		return nil, err
	}
	if cfg.AccountKey == "" {
		return nil, fmt.Errorf("the secret %s/%s has an empty value for REGISTRY_STORAGE_AZURE_ACCOUNTKEY; the secret should be removed so that the operator can use cluster-wide secrets or it should contain a valid storage account access key", sec.Namespace, sec.Name)
	}

	return cfg, nil
}

// GetConfig reads configuration for the Azure cloud platform services.
func GetConfig(listers *regopclient.Listers) (*Azure, error) {
	sec, err := listers.Secrets.Get(imageregistryv1.ImageRegistryPrivateConfigurationUser)
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("unable to get user provided secrets: %s", err)
		}

		creds, err := listers.Secrets.Get(imageregistryv1.CloudCredentialsName)
		if err != nil {
			return nil, fmt.Errorf("unable to get cluster minted credentials: %s", err)
		}
		return getAzureConfigFromCloudSecret(creds)
	}
	return getAzureConfigFromUserSecret(sec)
}

// generateAccountName returns a name that can be used for an Azure Storage
// Account. Storage account names must be between 3 and 24 characters in
// length and use numbers and lower-case letters only.
func generateAccountName(infrastructureName string) string {
	prefix := storageAccountInvalidCharRe.ReplaceAllString(infrastructureName, "")
	if prefix == "" {
		prefix = "imageregistry"
	}
	if len(prefix) > 24-5 {
		prefix = prefix[:24-5]
	}
	prefix = prefix + rand.String(5)
	return strings.ToLower(prefix)
}

func createStorageAccount(storageAccountsClient storage.AccountsClient, resourceGroupName, accountName, location string) error {
	klog.Infof("attempt to create azure storage account %s (resourceGroup=%q, location=%q)...", accountName, resourceGroupName, location)

	ctx := context.TODO()

	result, err := storageAccountsClient.CheckNameAvailability(
		ctx,
		storage.AccountCheckNameAvailabilityParameters{
			Name: to.StringPtr(accountName),
			Type: to.StringPtr("Microsoft.Storage/storageAccounts"),
		})
	if err != nil {
		return fmt.Errorf("storage account check-name-availability failed: %s", err)
	}
	if *result.NameAvailable != true {
		return &errNameNotAvailable{
			AccountName: accountName,
			Message:     *result.Message,
		}
	}

	future, err := storageAccountsClient.Create(
		ctx,
		resourceGroupName,
		accountName,
		storage.AccountCreateParameters{
			Kind:     storage.Storage,
			Location: to.StringPtr(location),
			Sku: &storage.Sku{
				Name: storage.StandardLRS,
			},
			AccountPropertiesCreateParameters: &storage.AccountPropertiesCreateParameters{},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to start creating storage account: %s", err)
	}

	// TODO: this may take up to 10 minutes
	err = future.WaitForCompletionRef(ctx, storageAccountsClient.Client)
	if err != nil {
		return fmt.Errorf("failed to finish creating storage account: %s", err)
	}

	_, err = future.Result(storageAccountsClient)
	if err != nil {
		return fmt.Errorf("failed to create storage account: %s", err)
	}

	klog.Infof("azure storage account %s has been created", accountName)

	return nil
}

func getAccountPrimaryKey(storageAccountsClient storage.AccountsClient, resourceGroupName, accountName string) (string, error) {
	ctx := context.TODO()

	keysResponse, err := storageAccountsClient.ListKeys(ctx, resourceGroupName, accountName)
	if err != nil {
		wrappedErr := fmt.Errorf("failed to get keys for the storage account %s: %s", accountName, err)
		if e, ok := err.(autorest.DetailedError); ok {
			if e.StatusCode == http.StatusNotFound {
				return "", &errDoesNotExist{Err: wrappedErr}
			}
		}
		return "", wrappedErr
	}

	return *(*keysResponse.Keys)[0].Value, nil
}

func getStorageContainer(accountName, key, containerName string) (azblob.ContainerURL, error) {
	c, err := azblob.NewSharedKeyCredential(accountName, key)
	if err != nil {
		return azblob.ContainerURL{}, err
	}

	p := azblob.NewPipeline(c, azblob.PipelineOptions{
		Telemetry: azblob.TelemetryOptions{Value: parameters.UserAgent},
	})

	u, err := url.Parse(fmt.Sprintf(blobFormatString, accountName))
	if err != nil {
		return azblob.ContainerURL{}, err
	}

	service := azblob.NewServiceURL(*u, p)
	return service.NewContainerURL(containerName), nil
}

func createStorageContainer(accountName, key, containerName string) error {
	ctx := context.TODO()

	container, err := getStorageContainer(accountName, key, containerName)
	if err != nil {
		return err
	}

	_, err = container.Create(ctx, azblob.Metadata{}, azblob.PublicAccessNone)
	return err
}

func deleteStorageContainer(accountName, key, containerName string) error {
	ctx := context.TODO()

	container, err := getStorageContainer(accountName, key, containerName)
	if err != nil {
		return err
	}

	_, err = container.Delete(ctx, azblob.ContainerAccessConditions{})
	return err
}

type driver struct {
	Config     *imageregistryv1.ImageRegistryConfigStorageAzure
	KubeConfig *rest.Config
	Listers    *regopclient.Listers
}

// NewDriver creates a new storage driver for Azure Blob Storage.
func NewDriver(c *imageregistryv1.ImageRegistryConfigStorageAzure, kubeconfig *rest.Config, listers *regopclient.Listers) *driver {
	return &driver{
		Config:     c,
		KubeConfig: kubeconfig,
		Listers:    listers,
	}
}

func (d *driver) storageAccountsClient(cfg *Azure) (storage.AccountsClient, error) {
	auth, err := auth.NewClientCredentialsConfig(cfg.ClientID, cfg.ClientSecret, cfg.TenantID).Authorizer()
	if err != nil {
		return storage.AccountsClient{}, err
	}

	storageAccountsClient := storage.NewAccountsClient(cfg.SubscriptionID)
	storageAccountsClient.Authorizer = auth
	storageAccountsClient.AddToUserAgent(parameters.UserAgent)

	return storageAccountsClient, nil
}

// ConfigEnv configures the environment variables that will be used in the
// image registry deployment.
func (d *driver) ConfigEnv() (envs []corev1.EnvVar, err error) {
	envs = append(envs,
		corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: "azure"},
		corev1.EnvVar{Name: "REGISTRY_STORAGE_AZURE_CONTAINER", Value: d.Config.Container},
		corev1.EnvVar{Name: "REGISTRY_STORAGE_AZURE_ACCOUNTNAME", Value: d.Config.AccountName},
		corev1.EnvVar{
			Name: "REGISTRY_STORAGE_AZURE_ACCOUNTKEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: imageregistryv1.ImageRegistryPrivateConfiguration,
					},
					Key: "REGISTRY_STORAGE_AZURE_ACCOUNTKEY",
				},
			},
		},
	)
	return
}

func (d *driver) Volumes() ([]corev1.Volume, []corev1.VolumeMount, error) {
	return nil, nil, nil
}

// Secrets returns a map of the storage access secrets.
func (d *driver) Secrets() (map[string]string, error) {
	cfg, err := GetConfig(d.Listers)
	if err != nil {
		return nil, err
	}

	key := cfg.AccountKey
	if key == "" {
		storageAccountsClient, err := d.storageAccountsClient(cfg)
		if err != nil {
			return nil, err
		}

		key, err = getAccountPrimaryKey(storageAccountsClient, cfg.ResourceGroup, d.Config.AccountName)
		if err != nil {
			return nil, err
		}
	}

	return map[string]string{
		"REGISTRY_STORAGE_AZURE_ACCOUNTKEY": key,
	}, nil
}

// StorageExists checks if the storage container exists and accessible.
func (d *driver) StorageExists(cr *imageregistryv1.Config) (bool, error) {
	if d.Config.AccountName == "" || d.Config.Container == "" {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonNotConfigured, "Storage is not configured")
		return false, nil
	}

	cfg, err := GetConfig(d.Listers)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonConfigError, fmt.Sprintf("Unable to get configuration: %s", err))
		return false, err
	}

	key := cfg.AccountKey
	if key == "" {
		storageAccountsClient, err := d.storageAccountsClient(cfg)
		if err != nil {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get accounts client: %s", err))
			return false, err
		}

		// TODO: get key from the generated secret?
		key, err = getAccountPrimaryKey(storageAccountsClient, cfg.ResourceGroup, d.Config.AccountName)
		if err != nil {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get account primary keys: %s", err))
			return false, err
		}
	}

	c, err := azblob.NewSharedKeyCredential(d.Config.AccountName, key)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to create shared key credential: %s", err))
		return false, err
	}

	u, err := url.Parse(fmt.Sprintf(blobFormatString, d.Config.AccountName))
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to parse blob URL: %s", err))
		return false, err
	}

	p := azblob.NewPipeline(c, azblob.PipelineOptions{
		Telemetry: azblob.TelemetryOptions{Value: parameters.UserAgent},
	})

	ctx := context.TODO()
	service := azblob.NewServiceURL(*u, p)
	container := service.NewContainerURL(d.Config.Container)
	_, err = container.GetProperties(ctx, azblob.LeaseAccessConditions{})
	if e, ok := err.(azblob.StorageError); ok {
		if e.ServiceCode() == azblob.ServiceCodeContainerNotFound {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonContainerNotFound, "Container does not exist")
			return false, nil
		}
	}
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get the storage container: %s", err))
		return false, fmt.Errorf("unable to get the storage container %s: %s", d.Config.Container, err)
	}

	util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionTrue, storageExistsReasonContainerExists, "Storage container exists")
	return true, nil
}

// StorageChanged checks if the storage configuration has changed.
func (d *driver) StorageChanged(cr *imageregistryv1.Config) bool {
	if !reflect.DeepEqual(cr.Status.Storage.Azure, cr.Spec.Storage.Azure) {
		return true
	}
	return false
}

// CreateStorage attempts to create a storage account and a storage container.
func (d *driver) CreateStorage(cr *imageregistryv1.Config) error {
	cfg, err := GetConfig(d.Listers)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonConfigError, fmt.Sprintf("Unable to get configuration: %s", err))
		return err
	}

	key := cfg.AccountKey
	if key != "" {
		// UPI
		if d.Config.AccountName == "" {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonNotConfigured, "Storage account key is provided, but account name is not specified")
			return nil
		}

		if d.Config.Container == "" {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonNotConfigured, "Storage account is provided, but container is not specified")
			return nil
		}

		cr.Status.StorageManaged = false
		cr.Status.Storage.Azure = d.Config.DeepCopy()
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionTrue, storageExistsReasonUserManaged, "Storage is managed by the user")
	} else {
		// IPI
		storageAccountsClient, err := d.storageAccountsClient(cfg)
		if err != nil {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get accounts client: %s", err))
			return err
		}

		if d.Config.AccountName == "" {
			infra, err := util.GetInfrastructure(d.Listers)
			if err != nil {
				util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonConfigError, fmt.Sprintf("Unable to get infrastructure resource: %s", err))
				return err
			}

			const maxAttempts = 10
			var lastErr error
			for i := 0; i < maxAttempts; i++ {
				accountName := generateAccountName(infra.Status.InfrastructureName)
				err = createStorageAccount(storageAccountsClient, cfg.ResourceGroup, accountName, cfg.Region)
				if err != nil {
					if _, ok := err.(*errNameNotAvailable); ok {
						klog.Warningf("unable to create storage account: %s", err)
						lastErr = err
						continue
					}
					return err
				}
				d.Config.AccountName = accountName
				cr.Status.StorageManaged = true
				cr.Status.Storage.Azure = d.Config.DeepCopy()
				cr.Spec.Storage.Azure = d.Config.DeepCopy()
				break
			}
			if d.Config.AccountName == "" {
				util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonAzureError, fmt.Sprintf("Unable to create storage account: %s", lastErr))
				return fmt.Errorf("attmpts to create storage account failed, last error: %s", lastErr)
			}
		} else {
			// TODO: do we need to create a storage account if we are provided with its name?
			err = createStorageAccount(storageAccountsClient, cfg.ResourceGroup, d.Config.AccountName, cfg.Region)
			if err != nil {
				if _, ok := err.(*errNameNotAvailable); !ok {
					util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to create storage account: %s", err))
					return err
				}

				// TODO: if the storage account already exists, do we need to check that we can use it?

				// The storage condition will be updated later.
			}
			cr.Status.Storage.Azure = d.Config.DeepCopy()
		}

		if d.Config.Container == "" {
			key, err := getAccountPrimaryKey(storageAccountsClient, cfg.ResourceGroup, d.Config.AccountName)
			if err != nil {
				util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonAzureError, fmt.Sprintf("Unable to get account primary key: %s", err))
				return err
			}

			containerName := "image-registry"

			err = createStorageContainer(d.Config.AccountName, key, containerName)
			if err != nil {
				util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to create storage container: %s", err))
				return fmt.Errorf("unable to create storage container: %s", err)
			}

			d.Config.Container = containerName
			cr.Status.StorageManaged = true
			cr.Status.Storage.Azure = d.Config.DeepCopy()
			cr.Spec.Storage.Azure = d.Config.DeepCopy()
		}

		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionTrue, storageExistsReasonContainerExists, "Storage container exists")
	}

	return nil
}

// RemoveStorage deletes the storage medium that was created.
func (d *driver) RemoveStorage(cr *imageregistryv1.Config) (retry bool, err error) {
	if cr.Status.StorageManaged != true {
		return false, nil
	}
	if d.Config.AccountName == "" {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonNotConfigured, "Storage is not configured")
		return false, nil
	}

	cfg, err := GetConfig(d.Listers)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonConfigError, fmt.Sprintf("Unable to get configuration: %s", err))
		return false, err
	}

	storageAccountsClient, err := d.storageAccountsClient(cfg)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get accounts client: %s", err))
		return false, err
	}

	if d.Config.Container != "" {
		key, err := getAccountPrimaryKey(storageAccountsClient, cfg.ResourceGroup, d.Config.AccountName)
		if _, ok := err.(*errDoesNotExist); ok {
			d.Config.AccountName = ""
			cr.Spec.Storage.Azure.AccountName = "" // TODO
			cr.Status.Storage.Azure.AccountName = ""
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonContainerNotFound, fmt.Sprintf("Container has been already deleted: %s", err))
			return false, nil
		}
		if err != nil {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to get account primary keys: %s", err))
			return false, err
		}

		err = deleteStorageContainer(d.Config.AccountName, key, d.Config.Container)
		if err != nil {
			util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionUnknown, storageExistsReasonAzureError, fmt.Sprintf("Unable to delete storage container: %s", err))
			return false, err // TODO: is it retryable?
		}

		d.Config.Container = ""
		cr.Spec.Storage.Azure.Container = "" // TODO: what if it was provided by a user?
		cr.Status.Storage.Azure.Container = ""
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonContainerDeleted, "Storage container has been deleted")
	}

	ctx := context.TODO()
	_, err = storageAccountsClient.Delete(ctx, cfg.ResourceGroup, d.Config.AccountName)
	if err != nil {
		util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonAzureError, fmt.Sprintf("Unable to delete storage account: %s", err))
		return false, err
	}

	d.Config.AccountName = ""
	cr.Spec.Storage.Azure.AccountName = "" // TODO
	cr.Status.Storage.Azure.AccountName = ""
	util.UpdateCondition(cr, imageregistryv1.StorageExists, operatorapiv1.ConditionFalse, storageExistsReasonAccountDeleted, "Storage account has been deleted")

	return false, nil
}
