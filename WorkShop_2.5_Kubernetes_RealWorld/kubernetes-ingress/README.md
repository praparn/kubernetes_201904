
[![Build Status](https://travis-ci.org/nginxinc/kubernetes-ingress.svg?branch=master)](https://travis-ci.org/nginxinc/kubernetes-ingress)  [![FOSSA Status](https://app.fossa.io/api/projects/custom%2B1062%2Fgithub.com%2Fnginxinc%2Fkubernetes-ingress.svg?type=shield)](https://app.fossa.io/projects/custom%2B1062%2Fgithub.com%2Fnginxinc%2Fkubernetes-ingress?ref=badge_shield)  [![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)  [![Go Report Card](https://goreportcard.com/badge/github.com/nginxinc/kubernetes-ingress)](https://goreportcard.com/report/github.com/nginxinc/kubernetes-ingress)

# NGINX Ingress Controller

This repo provides an implementation of an Ingress controller for NGINX and NGINX Plus. This implementation is different from the NGINX Ingress controller in [kubernetes/ingress-nginx](https://github.com/kubernetes/ingress-nginx) repo. See [this doc](docs/nginx-ingress-controllers.md) to find out about the key differences.

## What is Ingress?

An Ingress is a Kubernetes resource that lets you configure an HTTP load balancer for your Kubernetes services. Such a load balancer usually exposes your services to clients outside of your Kubernetes cluster. An Ingress resource supports:
* Exposing services:
    * Via custom URLs (for example, service A at the URL `/serviceA` and service B at the URL `/serviceB`).
    * Via multiple host names (for example, `foo.example.com` for one group of services and `bar.example.com` for another group).
* Configuring SSL termination for each exposed host name.

See the [Ingress User Guide](http://kubernetes.io/docs/user-guide/ingress/) to learn more.

## What is an Ingress Controller?

An Ingress controller is an application that monitors Ingress resources via the Kubernetes API and updates the configuration of a load balancer in case of any changes. Different load balancers require different Ingress controller implementations. Typically, an Ingress controller is deployed as a pod in a cluster. In the case of software load balancers, such as NGINX, an Ingress controller is deployed in a pod along with a load balancer.

See https://github.com/kubernetes/contrib/tree/master/ingress/controllers/ to learn more about Ingress controllers and find out about different implementations.

## NGINX Ingress Controller

We provide an Ingress controller for NGINX and NGINX Plus that supports the following Ingress features:
* SSL termination
* Path-based rules
* Multiple host names

We provide the following extensions to our Ingress controller:
* [Websocket](examples/websocket), which allows you to load balance Websocket applications.
* [SSL Services](examples/ssl-services), which allows you to load balance HTTPS applications.
* [Rewrites](examples/rewrites), which allows you to rewrite the URI of a request before sending it to the application.
* [Session Persistence](examples/session-persistence) (NGINX Plus only), which guarantees that all the requests from the same client are always passed to the same backend container.
* [Support for JWTs](examples/jwt) (NGINX Plus only), which allows NGINX Plus to authenticate requests by validating JSON Web Tokens (JWTs).

Additional extensions as well as a mechanism to customize NGINX configuration are available. See [ConfigMap and Annotations doc](docs/configmap-and-annotations.md).

## NGINX Ingress Controller Releases

We publish Ingress controller releases on GitHub. See our [releases page](https://github.com/nginxinc/kubernetes-ingress/releases).

The latest stable release is [1.3.1](https://github.com/nginxinc/kubernetes-ingress/releases/tag/v1.3.1). For production use, we recommend that you choose the latest stable release.  As an alternative, you can choose the *edge* version built from the [latest commit](https://github.com/nginxinc/kubernetes-ingress/commits/master) from the master branch. The edge version is useful for experimenting with new features that are not yet published in a stable release.

To use the Ingress controller, you need to have access to:
* An Ingress controller image.
* Installation manifests or a Helm chart.
* Documentation and examples.

It is important that the versions of those things above match. 

The table below summarizes the options regarding the images, manifests, helm chart, documentation and examples and gives your links to the correct versions:

| Version | Description |  Image for NGINX | Image for NGINX Plus | Installation Manifests and Helm Chart | Documentation and Examples |
| ------- | ----------- | --------------- | -------------------- | ---------------------------------------| -------------------------- |
| Latest stable release | For production use | `nginx/nginx-ingress:1.3.1`, `nginx/nginx-ingress:1.3.1-alpine` from [DockerHub](https://hub.docker.com/r/nginx/nginx-ingress/) or [build your own image](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/nginx-controller). | [Build your own image](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/nginx-controller). | [Manifests](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/install). [Helm chart](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/helm-chart). | [Documentation](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/docs). [Examples](https://github.com/nginxinc/kubernetes-ingress/tree/v1.3.1/examples). |
| Edge | For testing and experimenting | `nginx/nginx-ingress:edge`, `nginx/nginx-ingress:edge-alpine` from [DockerHub](https://hub.docker.com/r/nginx/nginx-ingress/) or [build your own image](https://github.com/nginxinc/kubernetes-ingress/tree/master/build). | [Build your own image](https://github.com/nginxinc/kubernetes-ingress/tree/master/build). | [Manifests](https://github.com/nginxinc/kubernetes-ingress/tree/master/deployments). [Helm chart](https://github.com/nginxinc/kubernetes-ingress/tree/master/deployments/helm-chart). | [Documentation](https://github.com/nginxinc/kubernetes-ingress/tree/master/docs). [Examples](https://github.com/nginxinc/kubernetes-ingress/tree/master/examples). |

## Benefits of Using the Ingress Controller with NGINX Plus

[NGINX Plus](https://www.nginx.com/products/) is a commercial version of NGINX that comes with advanced features and support.

The Ingress controller leverages the advanced features of NGINX Plus, which gives you the following additional benefits:

* **Improved system resources utilization for large-scale deployments**
Every time the number of pods of services you expose via Ingress changes, the Ingress controller updates the configuration of NGINX to reflect those changes. For the open source NGINX software, the configuration file must be changed and the configuration reloaded. For NGINX Plus, the [on-the-fly reconfiguration](https://www.nginx.com/products/on-the-fly-reconfiguration/) feature is utilized, which allows NGINX Plus to be updated on-the-fly without reloading the configuration. This prevents increase of memory usage during reloads, especially with a high volume of client requests, as well as increased memory usage when load balancing applications with long-lived connections (WebSocket, applications with file uploading/downloading or streaming). As a result, NGINX Plus Ingress controller is better suited for production-ready deployments.
* **Real-time statistics**
NGINX Plus provides you with [advanced statistics](https://www.nginx.com/products/live-activity-monitoring/), which you can access either through the API or via the built-in dashboard. This can give you insights into how NGINX Plus and your applications are performing.
* **Session persistence** When enabled, NGINX Plus makes sure that all the requests from the same client are always passed to the same backend container using the *sticky cookie* method. Refer to the [session persistence examples](examples/session-persistence) to find out how to configure it.
* **JWTs** NGINX Plus can validate JSON Web Tokens (JWTs), providing a flexible authentication mechanism.
* **Support** Support from NGINX Inc is available for NGINX Plus Ingress controller.

**Note**: Deployment of the Ingress controller for NGINX Plus requires you to do one extra step: build your own [Docker image](build) using the certificate and key for your subscription.
The Docker image of the Ingress controller for NGINX is [available on Docker Hub](https://hub.docker.com/r/nginx/nginx-ingress/).

## Using Multiple Ingress Controllers

You can run multiple Ingress controllers at the same time. For example, if your Kubernetes cluster is deployed in cloud, you can run the NGINX controller and the corresponding cloud HTTP load balancing controller. Refer to the [example](examples/multiple-ingress-controllers) to learn more.

## Advanced Load Balancing/An Alternative Method of Configuration

When your requirements go beyond what Ingress and Ingress extensions offer or if you are looking for an alternative method of configuring NGINX, it is possible to use NGINX or NGINX Plus without the Ingress Controller.

NGINX Plus comes with a [DNS-based dynamic reconfiguration feature](https://www.nginx.com/blog/dns-service-discovery-nginx-plus/), which lets you keep the list of the endpoints of your services in sync with NGINX Plus. Read more about how to setup NGINX Plus this way in [Load Balancing Kubernetes Services with NGINX Plus](https://www.nginx.com/blog/load-balancing-kubernetes-services-nginx-plus/).

## Contacts

We’d like to hear your feedback! If you have any suggestions or experience issues with our Ingress controller, please create an issue or send a pull request on Github.
You can contact us directly via [kubernetes@nginx.com](mailto:kubernetes@nginx.com).
