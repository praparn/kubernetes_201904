package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/nginxinc/kubernetes-ingress/internal/controller"
	"github.com/nginxinc/kubernetes-ingress/internal/handlers"
	"github.com/nginxinc/kubernetes-ingress/internal/nginx"
	"github.com/nginxinc/kubernetes-ingress/internal/nginx/plus"
	"github.com/nginxinc/kubernetes-ingress/internal/utils"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	// Set during build
	version   string
	gitCommit string

	healthStatus = flag.Bool("health-status", false,
		`Add a location "/nginx-health" to the default server. The location responds with the 200 status code for any request.
	Useful for external health-checking of the Ingress controller`)

	proxyURL = flag.String("proxy", "",
		`Use a proxy server to connect to Kubernetes API started by "kubectl proxy" command. For testing purposes only.
	The Ingress controller does not start NGINX and does not write any generated NGINX configuration files to disk`)

	watchNamespace = flag.String("watch-namespace", api_v1.NamespaceAll,
		`Namespace to watch for Ingress resources. By default the Ingress controller watches all namespaces`)

	nginxConfigMaps = flag.String("nginx-configmaps", "",
		`A ConfigMap resource for customizing NGINX configuration. If a ConfigMap is set,
	but the Ingress controller is not able to fetch it from Kubernetes API, the Ingress controller will fail to start.
	Format: <namespace>/<name>`)

	nginxPlus = flag.Bool("nginx-plus", false, "Enable support for NGINX Plus")

	ingressClass = flag.String("ingress-class", "nginx",
		`A class of the Ingress controller. The Ingress controller only processes Ingress resources that belong to its class
	- i.e. have the annotation "kubernetes.io/ingress.class" equal to the class. Additionally,
	the Ingress controller processes Ingress resources that do not have that annotation,
	which can be disabled by setting the "-use-ingress-class-only" flag`)

	useIngressClassOnly = flag.Bool("use-ingress-class-only", false,
		`Ignore Ingress resources without the "kubernetes.io/ingress.class" annotation`)

	defaultServerSecret = flag.String("default-server-tls-secret", "",
		`A Secret with a TLS certificate and key for TLS termination of the default server. Format: <namespace>/<name>.
	If not set, certificate and key in the file "/etc/nginx/secrets/default" are used. If a secret is set,
	but the Ingress controller is not able to fetch it from Kubernetes API or a secret is not set and
	the file "/etc/nginx/secrets/default" does not exist, the Ingress controller will fail to start`)

	versionFlag = flag.Bool("version", false, "Print the version and git-commit hash and exit")

	mainTemplatePath = flag.String("main-template-path", "",
		`Path to the main NGINX configuration template. (default for NGINX "nginx.tmpl"; default for NGINX Plus "nginx-plus.tmpl")`)

	ingressTemplatePath = flag.String("ingress-template-path", "",
		`Path to the ingress NGINX configuration template for an ingress resource.
	(default for NGINX "nginx.ingress.tmpl"; default for NGINX Plus "nginx-plus.ingress.tmpl")`)

	externalService = flag.String("external-service", "",
		`Specifies the name of the service with the type LoadBalancer through which the Ingress controller pods are exposed externally.
The external address of the service is used when reporting the status of Ingress resources. Requires -report-ingress-status.`)

	reportIngressStatus = flag.Bool("report-ingress-status", false,
		"Update the address field in the status of Ingresses resources. Requires the -external-service flag, or the 'external-status-address' key in the ConfigMap.")

	leaderElectionEnabled = flag.Bool("enable-leader-election", false,
		"Enable Leader election to avoid multiple replicas of the controller reporting the status of Ingress resources -- only one replica will report status. See -report-ingress-status flag.")

	nginxStatusAllowCIDRs = flag.String("nginx-status-allow-cidrs", "127.0.0.1", `Whitelist IPv4 IP/CIDR blocks to allow access to NGINX stub_status or the NGINX Plus API. Separate multiple IP/CIDR by commas.`)

	nginxStatusPort = flag.Int("nginx-status-port", 8080,
		"Set the port where the NGINX stub_status or the NGINX Plus API is exposed. [1023 - 65535]")

	nginxStatus = flag.Bool("nginx-status", true,
		"Enable the NGINX stub_status, or the NGINX Plus API.")

	nginxDebug = flag.Bool("nginx-debug", false,
		"Enable debugging for NGINX. Uses the nginx-debug binary. Requires 'error-log-level: debug' in the ConfigMap.")
)

func main() {
	flag.Parse()
	flag.Lookup("logtostderr").Value.Set("true")

	if *versionFlag {
		fmt.Printf("Version=%v GitCommit=%v\n", version, gitCommit)
		os.Exit(0)
	}

	portValidationError := validateStatusPort(*nginxStatusPort)
	if portValidationError != nil {
		glog.Fatalf("Invalid value for nginx-status-port: %v", portValidationError)
	}

	var err error
	allowedCIDRs, err := parseNginxStatusAllowCIDRs(*nginxStatusAllowCIDRs)
	if err != nil {
		glog.Fatalf(`Invalid value for nginx-status-allow-cidrs: %v`, err)
	}

	glog.Infof("Starting NGINX Ingress controller Version=%v GitCommit=%v\n", version, gitCommit)

	var config *rest.Config
	if *proxyURL != "" {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{},
			&clientcmd.ConfigOverrides{
				ClusterInfo: clientcmdapi.Cluster{
					Server: *proxyURL,
				},
			}).ClientConfig()
		if err != nil {
			glog.Fatalf("error creating client configuration: %v", err)
		}
	} else {
		if config, err = rest.InClusterConfig(); err != nil {
			glog.Fatalf("error creating client configuration: %v", err)
		}
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v.", err)
	}

	local := *proxyURL != ""

	nginxConfTemplatePath := "nginx.tmpl"
	nginxIngressTemplatePath := "nginx.ingress.tmpl"
	if *nginxPlus {
		nginxConfTemplatePath = "nginx-plus.tmpl"
		nginxIngressTemplatePath = "nginx-plus.ingress.tmpl"
	}

	if *mainTemplatePath != "" {
		nginxConfTemplatePath = *mainTemplatePath
	}
	if *ingressTemplatePath != "" {
		nginxIngressTemplatePath = *ingressTemplatePath
	}

	nginxBinaryPath := "/usr/sbin/nginx"
	if *nginxDebug {
		nginxBinaryPath = "/usr/sbin/nginx-debug"
	}

	templateExecutor, err := nginx.NewTemplateExecutor(nginxConfTemplatePath, nginxIngressTemplatePath, *healthStatus, *nginxStatus, allowedCIDRs, *nginxStatusPort)
	if err != nil {
		glog.Fatalf("Error creating TemplateExecutor: %v", err)
	}
	ngxc := nginx.NewNginxController("/etc/nginx/", nginxBinaryPath, local)

	if *defaultServerSecret != "" {
		ns, name, err := utils.ParseNamespaceName(*defaultServerSecret)
		if err != nil {
			glog.Fatalf("Error parsing the default-server-tls-secret argument: %v", err)
		}
		secret, err := kubeClient.CoreV1().Secrets(ns).Get(name, meta_v1.GetOptions{})
		if err != nil {
			glog.Fatalf("Error when getting %v: %v", *defaultServerSecret, err)
		}
		err = nginx.ValidateTLSSecret(secret)
		if err != nil {
			glog.Fatalf("%v is invalid: %v", *defaultServerSecret, err)
		}

		bytes := nginx.GenerateCertAndKeyFileContent(secret)
		ngxc.AddOrUpdateSecretFile(nginx.DefaultServerSecretName, bytes, nginx.TLSSecretFileMode)
	} else {
		_, err = os.Stat("/etc/nginx/secrets/default")
		if os.IsNotExist(err) {
			glog.Fatalf("A TLS cert and key for the default server is not found")
		}
	}

	cfg := nginx.NewDefaultConfig()
	if *nginxConfigMaps != "" {
		ns, name, err := utils.ParseNamespaceName(*nginxConfigMaps)
		if err != nil {
			glog.Fatalf("Error parsing the nginx-configmaps argument: %v", err)
		}
		cfm, err := kubeClient.CoreV1().ConfigMaps(ns).Get(name, meta_v1.GetOptions{})
		if err != nil {
			glog.Fatalf("Error when getting %v: %v", *nginxConfigMaps, err)
		}
		cfg = nginx.ParseConfigMap(cfm, *nginxPlus)
		if cfg.MainServerSSLDHParamFileContent != nil {
			fileName, err := ngxc.AddOrUpdateDHParam(*cfg.MainServerSSLDHParamFileContent)
			if err != nil {
				glog.Fatalf("Configmap %s/%s: Could not update dhparams: %v", ns, name, err)
			} else {
				cfg.MainServerSSLDHParam = fileName
			}
		}
		if cfg.MainTemplate != nil {
			err = templateExecutor.UpdateMainTemplate(cfg.MainTemplate)
			if err != nil {
				glog.Fatalf("Error updating NGINX main template: %v", err)
			}
		}
		if cfg.IngressTemplate != nil {
			err = templateExecutor.UpdateIngressTemplate(cfg.IngressTemplate)
			if err != nil {
				glog.Fatalf("Error updating ingress template: %v", err)
			}
		}
	}

	ngxConfig := nginx.GenerateNginxMainConfig(cfg)
	content, err := templateExecutor.ExecuteMainConfigTemplate(ngxConfig)
	if err != nil {
		glog.Fatalf("Error generating NGINX main config: %v", err)
	}
	ngxc.UpdateMainConfigFile(content)
	ngxc.UpdateConfigVersionFile()

	nginxDone := make(chan error, 1)
	ngxc.Start(nginxDone)

	var nginxAPI *plus.NginxAPIController
	if *nginxPlus {
		httpClient := getSocketClient()
		nginxAPI, err = plus.NewNginxAPIController(&httpClient, "http://nginx-plus-api/api", local)
		if err != nil {
			glog.Fatalf("Failed to create NginxAPIController: %v", err)
		}
	}

	cnf := nginx.NewConfigurator(ngxc, cfg, nginxAPI, templateExecutor)
	controllerNamespace := os.Getenv("POD_NAMESPACE")

	lbcInput := controller.NewLoadBalancerControllerInput{
		KubeClient:              kubeClient,
		ResyncPeriod:            30 * time.Second,
		Namespace:               *watchNamespace,
		NginxConfigurator:       cnf,
		DefaultServerSecret:     *defaultServerSecret,
		IsNginxPlus:             *nginxPlus,
		IngressClass:            *ingressClass,
		UseIngressClassOnly:     *useIngressClassOnly,
		ExternalServiceName:     *externalService,
		ControllerNamespace:     controllerNamespace,
		ReportIngressStatus:     *reportIngressStatus,
		IsLeaderElectionEnabled: *leaderElectionEnabled,
	}

	lbc := controller.NewLoadBalancerController(lbcInput)

	// create handlers for resources we care about
	ingressHandlers := handlers.CreateIngressHandlers(lbc)
	secretHandlers := handlers.CreateSecretHandlers(lbc)
	serviceHandlers := handlers.CreateServiceHandlers(lbc)
	endpointHandlers := handlers.CreateEndpointHandlers(lbc)

	lbc.AddSecretHandler(secretHandlers)
	lbc.AddIngressHandler(ingressHandlers)
	lbc.AddServiceHandler(serviceHandlers)
	lbc.AddEndpointHandler(endpointHandlers)

	if *nginxConfigMaps != "" {
		nginxConfigMapsNS, nginxConfigMapsName, err := utils.ParseNamespaceName(*nginxConfigMaps)
		if err != nil {
			glog.Warning(err)
		} else {
			lbc.WatchNginxConfigMaps()
			configMapHandlers := handlers.CreateConfigMapHandlers(lbc, nginxConfigMapsName)
			lbc.AddConfigMapHandler(configMapHandlers, nginxConfigMapsNS)
		}
	}

	if lbcInput.ReportIngressStatus && lbcInput.IsLeaderElectionEnabled {
		leaderHandler := handlers.CreateLeaderHandler(lbc)
		lbc.AddLeaderHandler(leaderHandler)
	}

	go handleTermination(lbc, ngxc, nginxDone)
	lbc.Run()

	for {
		glog.Info("Waiting for the controller to exit...")
		time.Sleep(30 * time.Second)
	}
}

func handleTermination(lbc *controller.LoadBalancerController, ngxc *nginx.Controller, nginxDone chan error) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)

	exitStatus := 0
	exited := false

	select {
	case err := <-nginxDone:
		if err != nil {
			glog.Errorf("nginx command exited with an error: %v", err)
			exitStatus = 1
		} else {
			glog.Info("nginx command exited successfully")
		}
		exited = true
	case <-signalChan:
		glog.Infof("Received SIGTERM, shutting down")
	}

	glog.Infof("Shutting down the controller")
	lbc.Stop()

	if !exited {
		glog.Infof("Shutting down NGINX")
		ngxc.Quit()
		<-nginxDone
	}

	glog.Infof("Exiting with a status: %v", exitStatus)
	os.Exit(exitStatus)
}

// getSocketClient gets an http.Client with the a unix socket transport.
func getSocketClient() http.Client {
	return http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", "/var/run/nginx-plus-api.sock")
			},
		},
	}
}

func validateStatusPort(nginxStatusPort int) error {
	if nginxStatusPort < 1023 || nginxStatusPort > 65535 {
		return fmt.Errorf("port outside of valid port range [1023 - 65535]: %v", nginxStatusPort)
	}
	return nil
}

// parseNginxStatusAllowCIDRs converts a comma separated CIDR/IP address string into an array of CIDR/IP addresses.
// It returns an array of the valid CIDR/IP addresses or an error if given an invalid address.
func parseNginxStatusAllowCIDRs(input string) (cidrs []string, err error) {
	cidrsArray := strings.Split(input, ",")
	for _, cidr := range cidrsArray {
		trimmedCidr := strings.TrimSpace(cidr)
		err := validateCIDRorIP(trimmedCidr)
		if err != nil {
			return cidrs, err
		}
		cidrs = append(cidrs, trimmedCidr)
	}
	return cidrs, nil
}

// validateCIDRorIP makes sure a given string is either a valid CIDR block or IP address.
// It an error if it is not valid.
func validateCIDRorIP(cidr string) error {
	if cidr == "" {
		return fmt.Errorf("invalid CIDR address: an empty string is an invalid CIDR block or IP address")
	}
	_, _, err := net.ParseCIDR(cidr)
	if err == nil {
		return nil
	}
	ip := net.ParseIP(cidr)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %v", cidr)
	}
	return nil
}
