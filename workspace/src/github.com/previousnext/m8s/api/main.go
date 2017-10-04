package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	log "github.com/Sirupsen/logrus"
	"github.com/alecthomas/kingpin"
	"github.com/previousnext/m8s/api/k8s/addons/ssh-server"
	"github.com/previousnext/m8s/api/k8s/addons/traefik"
	"github.com/previousnext/m8s/api/k8s/env"
	"github.com/previousnext/m8s/api/k8s/utils"
	pb "github.com/previousnext/m8s/pb"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/api/v1"
	client "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
)

var (
	cliPort      = kingpin.Flag("port", "Port to run this service on").Default("443").OverrideDefaultFromEnvar("M8S_PORT").Int32()
	cliCert      = kingpin.Flag("cert", "Certificate for TLS connection").Default("").OverrideDefaultFromEnvar("M8S_TLS_CERT").String()
	cliKey       = kingpin.Flag("key", "Private key for TLS connection").Default("").OverrideDefaultFromEnvar("M8S_TLS_KEY").String()
	cliToken     = kingpin.Flag("token", "Token to authenticate against the API.").Default("").OverrideDefaultFromEnvar("M8S_AUTH_TOKEN").String()
	cliNamespace = kingpin.Flag("namespace", "Namespace to build environments.").Default("default").OverrideDefaultFromEnvar("M8S_NAMESPACE").String()

	cliFilesystemSize = kingpin.Flag("fs-size", "Size of the filesystem for persistent storage").Default("100Gi").OverrideDefaultFromEnvar("M8S_FS_SIZE").String()

	// Lets Encrypt.
	cliLetsEncryptEmail  = kingpin.Flag("lets-encrypt-email", "Email address to register with Lets Encrypt certificate").Default("admin@previousnext.com.au").OverrideDefaultFromEnvar("M8S_LETS_ENCRYPT_EMAIL").String()
	cliLetsEncryptDomain = kingpin.Flag("lets-encrypt-domain", "Domain to use for Lets Encrypt certificate").Default("").OverrideDefaultFromEnvar("M8S_LETS_ENCRYPT_DOMAIN").String()
	cliLetsEncryptCache  = kingpin.Flag("lets-encrypt-cache", "Cache directory to use for Lets Encrypt").Default("/tmp").OverrideDefaultFromEnvar("M8S_LETS_ENCRYPT_CACHE").String()

	// Traefik.
	cliTraefikImage   = kingpin.Flag("traefik-image", "Traefik image to deploy").Default("traefik").OverrideDefaultFromEnvar("M8S_TRAEFIK_IMAGE").String()
	cliTraefikVersion = kingpin.Flag("traefik-version", "Version of Traefik to deploy").Default("1.3").OverrideDefaultFromEnvar("M8S_TRAEFIK_VERSION").String()
	cliTraefikPort    = kingpin.Flag("traefik-port", "Assign this port to each node on the cluster for Traefik ingress").Default("80").OverrideDefaultFromEnvar("M8S_TRAEFIK_PORT").Int32()

	// SSH Server.
	cliSSHImage   = kingpin.Flag("ssh-image", "SSH server image to deploy").Default("previousnext/k8s-ssh-server").OverrideDefaultFromEnvar("M8S_SSH_IMAGE").String()
	cliSSHVersion = kingpin.Flag("ssh-version", "Version of SSH server to deploy").Default("2.1.0").OverrideDefaultFromEnvar("M8S_SSH_VERSION").String()

	// DockerCfg.
	cliDockerCfgRegistry = kingpin.Flag("dockercfg-registry", "Registry for Docker Hub credentials").Default("").OverrideDefaultFromEnvar("M8S_DOCKERCFG_REGISTRY").String()
	cliDockerCfgUsername = kingpin.Flag("dockercfg-username", "Username for Docker Hub credentials").Default("").OverrideDefaultFromEnvar("M8S_DOCKERCFG_USERNAME").String()
	cliDockerCfgPassword = kingpin.Flag("dockercfg-password", "Password for Docker Hub credentials").Default("").OverrideDefaultFromEnvar("M8S_DOCKERCFG_PASSWORD").String()
	cliDockerCfgEmail    = kingpin.Flag("dockercfg-email", "Email for Docker Hub credentials").Default("").OverrideDefaultFromEnvar("M8S_DOCKERCFG_EMAIL").String()
	cliDockerCfgAuth     = kingpin.Flag("dockercfg-auth", "Auth token for Docker Hub credentials").Default("").OverrideDefaultFromEnvar("M8S_DOCKERCFG_AUTH").String()

	// Promtheus.
	cliPrometheusPort   = kingpin.Flag("prometheus-port", "Prometheus metrics port").Default(":9000").OverrideDefaultFromEnvar("M8S_METRICS_PORT").String()
	cliPrometheusPath   = kingpin.Flag("prometheus-path", "Prometheus metrics path").Default("/metrics").OverrideDefaultFromEnvar("M8S_METRICS_PATH").String()
	cliPrometheusApache = kingpin.Flag("prometheus-apache-exporter", "Prometheus metrics port for Apache on built environments").Default("9117").OverrideDefaultFromEnvar("M8S_METRICS_APACHE_PORT").Int32()
)

func main() {
	kingpin.Parse()

	log.Println("Starting Prometheus Endpoint")

	go metrics(*cliPrometheusPort, *cliPrometheusPath)

	log.Println("Starting Server")

	listen, err := net.Listen("tcp", fmt.Sprintf(":%d", *cliPort))
	if err != nil {
		panic(err)
	}

	// Attempt to connect to the K8s API Server.
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}

	client, err := client.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	log.Println("Installing addon: traefik")

	err = traefik.Create(client, *cliNamespace, *cliTraefikImage, *cliTraefikVersion, *cliTraefikPort)
	if err != nil {
		panic(err)
	}

	log.Println("Installing addon: ssh-server")

	err = ssh_server.Create(client, *cliNamespace, *cliSSHImage, *cliSSHVersion)
	if err != nil {
		panic(err)
	}

	log.Println("Syncing secrets: dockercfg")

	err = dockercfgSync(client, *cliDockerCfgRegistry, *cliDockerCfgUsername, *cliDockerCfgPassword, *cliDockerCfgEmail, *cliDockerCfgAuth)
	if err != nil {
		panic(err)
	}

	log.Println("Booting API")

	// Create a new server which adheres to the GRPC interface.
	srv := server{
		client: client,
		config: config,
	}

	var creds credentials.TransportCredentials

	// Attempt to load user provided certificates.
	// If no certificates are provided, fallback to Lets Encrypt.
	if *cliCert != "" && *cliKey != "" {
		creds, err = credentials.NewServerTLSFromFile(*cliCert, *cliKey)
		if err != nil {
			panic(err)
		}
	} else {
		creds, err = getLetsEncrypt(*cliLetsEncryptDomain, *cliLetsEncryptEmail, *cliLetsEncryptCache)
		if err != nil {
			panic(err)
		}
	}

	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterM8SServer(grpcServer, srv)
	grpcServer.Serve(listen)
}

// Helper function for adding Lets Encrypt certificates.
func getLetsEncrypt(domain, email, cache string) (credentials.TransportCredentials, error) {
	manager := autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cache),
		HostPolicy: autocert.HostWhitelist(domain),
		Email:      email,
	}
	return credentials.NewTLS(&tls.Config{GetCertificate: manager.GetCertificate}), nil
}

// Helper function to sync Docker credentials.
func dockercfgSync(client *client.Clientset, registry, username, password, email, auth string) error {
	auths := map[string]DockerConfig{
		registry: {
			Username: username,
			Password: password,
			Email:    email,
			Auth:     auth,
		},
	}

	dockerconfig, err := json.Marshal(auths)
	if err != nil {
		return err
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: *cliNamespace,
			Name:      env.SecretDockerCfg,
		},
		Data: map[string][]byte{
			keyDockerCfg: dockerconfig,
		},
		Type: v1.SecretTypeDockercfg,
	}

	_, err = utils.SecretCreate(client, secret)
	if err != nil {
		return err
	}

	return nil
}

// Helper function for serving Prometheus metrics.
func metrics(port, path string) {
	http.Handle(path, promhttp.Handler())
	log.Fatal(http.ListenAndServe(port, nil))
}
