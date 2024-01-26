package pingdomtransaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pingdomNew "github.com/karlderkaefer/pingdom-golang-client/pkg/pingdom/openapi"
	"github.com/karlderkaefer/pingdom-golang-client/pkg/pingdom/openapi/ptr"
	endpointmonitorv1alpha1 "github.com/stakater/IngressMonitorController/v2/api/v1alpha1"
	"github.com/stakater/IngressMonitorController/v2/pkg/config"
	"github.com/stakater/IngressMonitorController/v2/pkg/kube"
	"github.com/stakater/IngressMonitorController/v2/pkg/models"
	"github.com/stakater/IngressMonitorController/v2/pkg/util"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

var log = logf.Log.WithName("pingdom-transaction")

// PingdomTransactionMonitorService interfaces with MonitorService
type PingdomTransactionMonitorService struct {
	apiToken          string
	url               string
	alertContacts     string
	alertIntegrations string
	teamAlertContacts string
	client            *pingdomNew.APIClient
	context           context.Context
	kubeClient        *kubernetes.Clientset
	namespace         string
}

func (monitor *PingdomTransactionMonitorService) Equal(oldMonitor models.Monitor, newMonitor models.Monitor) bool {
	// TODO: Retrieve oldMonitor config and compare it here
	return false
}

func (service *PingdomTransactionMonitorService) Setup(p config.Provider) {
	service.apiToken = p.ApiToken
	service.url = p.ApiURL
	service.alertContacts = p.AlertContacts
	service.alertIntegrations = p.AlertIntegrations
	service.teamAlertContacts = p.TeamAlertContacts

	cfg := pingdomNew.NewConfiguration()
	if service.apiToken == "" {
		service.apiToken = os.Getenv("PINGDOM_API_TOKEN")
	}
	cfg.SetApiToken(service.apiToken)
	service.client = pingdomNew.NewAPIClient(cfg)
	service.context = context.Background()
	kubeClient, err := kube.GetClient()
	if err != nil {
		log.Error(err, "Error creating kubernetes client")
	}
	service.kubeClient = kubeClient
	service.namespace = kube.GetCurrentKubernetesNamespace()
}

func (service *PingdomTransactionMonitorService) GetByName(name string) (*models.Monitor, error) {
	var match *models.Monitor

	monitors := service.GetAll()
	for _, mon := range monitors {
		if mon.Name == name {
			return &mon, nil
		}
	}

	return match, fmt.Errorf("unable to locate monitor with name %v", name)
}

func (service *PingdomTransactionMonitorService) GetAll() []models.Monitor {
	var monitors []models.Monitor
	checks, _, err := service.client.TMSChecksAPI.GetAllChecks(service.context).Type_("script").Execute()
	if err != nil {
		log.Error(err, "Error getting all transaction checks")
		return monitors
	}

	if checks == nil {
		return monitors
	}
	for _, mon := range checks.GetChecks() {
		newMon := models.Monitor{
			URL:  service.GetUrlFromSteps(*mon.Id),
			ID:   fmt.Sprintf("%v", *mon.Id),
			Name: *mon.Name,
		}
		monitors = append(monitors, newMon)
	}
	return monitors
}

func (service *PingdomTransactionMonitorService) GetUrlFromSteps(id int64) string {
	check, _, err := service.client.TMSChecksAPI.GetCheck(service.context, id).Execute()
	if err != nil {
		log.Error(err, "Error getting transaction check")
		return ""
	}
	if check == nil {
		return ""
	}
	for _, step := range check.GetSteps() {
		if step.GetFn() == "go_to" {
			return *step.GetArgs().Url
		}
	}
	return ""
}

func (service *PingdomTransactionMonitorService) Add(m models.Monitor) {
	transactionCheck := service.createTransactionCheck(m)
	if transactionCheck == nil {
		return
	}
	_, resp, err := service.client.TMSChecksAPI.AddCheck(service.context).CheckWithoutID(*transactionCheck).Execute()
	if err != nil {
		log.Error(err, "Error Adding Pingdom Transaction Monitor "+m.Name, "Response", parseResponseBody(resp))
	} else {
		log.Info("Successfully added Pingdom Transaction Monitor " + m.Name)
	}
}

func (service *PingdomTransactionMonitorService) Update(m models.Monitor) {
	transactionCheck := service.createTransactionCheck(m)
	if transactionCheck == nil {
		return
	}
	monitorID := util.StrToInt64(m.ID)
	_, resp, err := service.client.TMSChecksAPI.ModifyCheck(service.context, monitorID).CheckWithoutIDPUT(*transactionCheck.AsPut()).Execute()
	if err != nil {
		log.Error(err, "Error Updating Pingdom Transaction Monitor", "Response", parseResponseBody(resp))
		return
	}
	log.Info("Updated Pingdom Transaction Monitor Monitor " + m.Name)
}

func (service *PingdomTransactionMonitorService) Remove(m models.Monitor) {
	_, resp, err := service.client.TMSChecksAPI.DeleteCheck(service.context, util.StrToInt64(m.ID)).Execute()
	if err != nil {
		log.Error(err, "Error Deleting Pingdom Transaction Monitor", "Response", parseResponseBody(resp))
	} else {
		log.Info("Deleted Pingdom Transaction Monitor Monitor " + m.Name)
	}
}

func (service *PingdomTransactionMonitorService) createTransactionCheck(monitor models.Monitor) *pingdomNew.CheckWithoutID {
	transactionCheck := &pingdomNew.CheckWithoutID{}
	providerConfig, _ := monitor.Config.(*endpointmonitorv1alpha1.PingdomTransactionConfig)
	if providerConfig == nil {
		// ignore monitor if type is not PingdomTransaction
		log.Info("Monitor is not PingdomTransaction type" + monitor.Name)
		return nil
	}
	transactionCheck.Name = monitor.Name

	userIds := parseIDs(service.alertContacts)
	if userIds != nil {
		transactionCheck.ContactIds = userIds
	}
	integrationIds := parseIDs(service.alertIntegrations)
	if integrationIds != nil {
		transactionCheck.IntegrationIds = integrationIds
	}
	teamAlertContacts := parseIDs(service.teamAlertContacts)
	if teamAlertContacts != nil {
		transactionCheck.TeamIds = teamAlertContacts
	}
	service.addConfigToTranscationCheck(transactionCheck, monitor)

	return transactionCheck
}

func (service *PingdomTransactionMonitorService) addConfigToTranscationCheck(transactionCheck *pingdomNew.CheckWithoutID, monitor models.Monitor) {

	// Retrieve provider configuration
	config := monitor.Config
	providerConfig, _ := config.(*endpointmonitorv1alpha1.PingdomTransactionConfig)

	if providerConfig == nil {
		// providerConfig is not set, we create a go_to transaction by default from url because its required by API
		transactionCheck.Steps = append(transactionCheck.Steps, pingdomNew.Step{
			Args: &pingdomNew.StepArgs{
				Url: ptr.String(monitor.URL),
			},
			Fn: ptr.String("go_to"),
		})
		return
	}

	// Set contact ids
	userIds := parseIDs(providerConfig.AlertContacts)
	if userIds != nil {
		transactionCheck.ContactIds = userIds
	}
	teamAlertContacts := parseIDs(providerConfig.TeamAlertContacts)
	if teamAlertContacts != nil {
		transactionCheck.TeamIds = teamAlertContacts
	}
	integrationIds := parseIDs(providerConfig.AlertIntegrations)
	if integrationIds != nil {
		transactionCheck.IntegrationIds = integrationIds
	}

	// Set transaction check configuration
	if providerConfig.CustomMessage != "" {
		transactionCheck.CustomMessage = ptr.String(providerConfig.CustomMessage)
	}
	if providerConfig.Region != "" {
		transactionCheck.Region = ptr.String(providerConfig.Region)
	}
	if providerConfig.SendNotificationWhenDown > 0 {
		transactionCheck.SendNotificationWhenDown = ptr.Int64(providerConfig.SendNotificationWhenDown)
	}
	if providerConfig.Paused {
		transactionCheck.Active = ptr.Bool(!providerConfig.Paused)
	}
	if len(providerConfig.Tags) > 0 {
		transactionCheck.Tags = providerConfig.Tags
	}
	if providerConfig.SeverityLevel != "" {
		transactionCheck.SeverityLevel = ptr.String(providerConfig.SeverityLevel)
	}
	if providerConfig.Interval > 0 {
		transactionCheck.Interval = ptr.Int64((int64(providerConfig.Interval)))
	}
	for _, step := range providerConfig.Steps {
		args := service.NewStepArgsByMap(step.Args)
		if args != nil {
			transactionCheck.Steps = append(transactionCheck.Steps, pingdomNew.Step{
				Args: args,
				Fn:   ptr.String(step.Function),
			})
		} else {
			log.Info("Invalid Pingdom Step Args Provided")
		}
	}
}

// KubernetesInterface abstracts the clientset methods used
type KubernetesInterface interface {
	CoreV1() corev1.CoreV1Interface
}

// Ensure the real clientset satisfies the interface
var _ KubernetesInterface = &kubernetes.Clientset{}

// Ensure the fake clientset satisfies the interface
var _ KubernetesInterface = &fake.Clientset{}

// NewStepArgsByMap creates a new StepArgs object from a map
func (service *PingdomTransactionMonitorService) NewStepArgsByMap(input map[string]string) *pingdomNew.StepArgs {
	// First, marshal the map to JSON
	jsonData, err := json.Marshal(input)
	if err != nil {
		log.Error(err, "Error marshalling map to JSON")
		return nil
	}
	var stepArgs pingdomNew.StepArgs
	err = json.Unmarshal(jsonData, &stepArgs)
	if err != nil {
		log.Error(err, "Error marshalling map to StepArgs")
		return nil
	}
	// Replace secrets in the password field
	err = replaceSecretValuesInArgs(&stepArgs, service.kubeClient, service.namespace)
	if err != nil {
		log.Error(err, "Error replacing secrets in step args")
		return nil
	}
	return &stepArgs
}

// ReplaceSecretValuesInArgs replaces secrets in StepArgs with actual secret values from Kubernetes.
// It expects secrets to be formatted as {{secret:secret-name:key}} in the args.
// Returns an error if the secret or defined secret key cannot be retrieved.
func replaceSecretValuesInArgs(args *pingdomNew.StepArgs, kubeClient KubernetesInterface, namespace string) error {
	if args == nil {
		return nil // No arguments provided
	}
	// Helper function to process a single string field
	processField := func(field *string) error {
		if field == nil {
			return nil // Skip nil fields
		}
		secretName, secretKey := parseSecretTemplate(*field)
		if secretName != "" && secretKey != "" {
			secretValue, err := getSecretValue(kubeClient, namespace, secretName, secretKey)

			if err != nil {
				return fmt.Errorf("failed to get secret: %v", err)
			}
			*field = secretValue
		}
		return nil
	}
	// Process the Password and Input fields
	if err := processField(args.Password); err != nil {
		return err
	}
	if err := processField(args.Value); err != nil {
		return err
	}

	return nil
}

// parseSecretTemplate extracts secret name and key from a string template.
func parseSecretTemplate(content string) (secretName string, secretKey string) {
	const secretPattern = `{{secret:(.*):(.*)}}`
	re := regexp.MustCompile(secretPattern)

	matches := re.FindStringSubmatch(content)
	if len(matches) == 3 {
		return matches[1], matches[2]
	}
	return "", ""
}

// getSecretValue retrieves a secret value from Kubernetes.
func getSecretValue(kubeClient KubernetesInterface, namespace, secretName, secretKey string) (string, error) {
	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(context.Background(), secretName, v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to retrieve secret %s: %v", secretName, err)
	}

	secretValue, ok := secret.Data[secretKey]
	if !ok {
		return "", fmt.Errorf("secret %s does not contain key %s", secretName, secretKey)
	}

	return string(secretValue), nil
}

// parseIDs splits a string of IDs into an array of integers
func parseIDs(field string) []int64 {
	if len(field) > 0 {
		stringArray := strings.Split(field, "-")
		ids, err := util.SliceAtoi64(stringArray)
		if err != nil {
			log.Error(err, "Error decoding ids into integers")
			return nil
		}
		return ids
	}
	return nil
}

// parseResponseBody checks if the response body is JSON and contains "errormessage".
// If so, it returns the value of "errormessage". Otherwise, it returns the entire body.
func parseResponseBody(resp *http.Response) string {
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error(err, "Error reading response body")
		return ""
	}
	// Attempt to unmarshal the response body into a map
	var responseBodyMap map[string]map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &responseBodyMap); err != nil {
		// If unmarshaling fails, return the whole body as a string.
		return string(bodyBytes)
	}
	// Check if "error" key exists in the map
	if errorObj, ok := responseBodyMap["error"]; ok {
		// Check if "errormessage" key exists in the "error" object
		if errorMessage, ok := errorObj["errormessage"]; ok {
			if errMsgStr, ok := errorMessage.(string); ok {
				return errMsgStr
			}
		}
	}
	// If "errormessage" key doesn't exist or isn't a string, return the whole JSON body
	return string(bodyBytes)
}
