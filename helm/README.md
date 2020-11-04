# Ingress Merge Helm Chart

---

Installs [Ingress Merge](../) to a Kubernetes cluster

---

## Table of Contents

---

- [Introduction](#introduction)
- [Prerequisites](#prerequisites)
- [Installing the Chart](#installing-the-chart)
- [Configuration](#configuration)

---

## Introduction

---

This chart stands up the FlowJo Hub API.

---

## Prerequisites

---

- Helm v3.2.1+
- Kubernetes v1.18+

---

## Installing the Chart

---

This chart can be installed as given with no further configuration.

The ingress class to monitor can be modified from its default `merge` if desired.

Options are available through labels and annotations for additional fine-tuning
of what configmaps and ingresses may be monitored and modified.

---

## Configuration

---
The following table lists the configurable parameters of the chart and their
default values.

Parameter | Description | Default
--- | --- | ---
`image.repository` | Image repository and name | `shaunrasmusen/ingress-merge`
`image.tag` | Image tag  | `latest`
`image.pullPolicy` | Image pull policy  | `Always`
`extraArgs` | Extra arguments to pass to the container | `{}`
`ingressClass` | ingress class to match | `merge`
`configMapSelector` | Label selector for ConfigMap objects to be monitored for changes | `''`
`ingressSelector` | Label selector for Ingress objects to be monitored for changes | `''`
`configMapWatchIgnore` | List of annotations that will cause a ConfigMap to be ignored if present | `[]`
`ingressWatchIgnore` | List of annotations that will cause an Ingress to be ignored if present | `[]`
`imagePullSecrets` | secrets needed to access the image | `[]`
`priorityClassName` | priority class for the deployment | `''`
`podSecurityContext` | a security context for the pods | `{}`
`securityContext` | a security context for the container | `{}`
`nodeSelector` | Node selectors | `{}`
`tolerations` | Node tolerations | `[]`
`affinity` | Pod affinities | `{}`
`podAnnotations` | annotations to add to the pod | `{}`
`podLabels` | labels to add to the pod | `{}`
`rbac.create` | If true, create & use RBAC resources | `true`
`rbac.serviceAccountName` | existing ServiceAccount to use (ignored if rbac.create=true) | `default`
`rbac.podSecurityPolicy` | whether to create and attach a pod security policy | `false`
`resources` | pod resource requests & limits | `{}`

Specify each parameter you'd like to override using a YAML file as described
above in the [installation](#installation) section or by using the
`--set key=value[,key=value]` argument to `helm install`. For example, to change
 the name of the script content type:

```console
$ helm install --name my-release .
```

