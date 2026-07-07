variable "location" {
  description = "Azure region"
  type        = string
  default     = "westeurope"
}

variable "resource_group_name" {
  type    = string
  default = "rg-nydus-cache"
}

variable "cluster_name" {
  type    = string
  default = "aks-nydus-cache"
}

# ACR names are GLOBAL and alphanumeric-only; adjust on a 409 conflict.
variable "acr_name" {
  type    = string
}

variable "node_count" {
  type    = number
  default = 3
}

# Standard_D2s_v5 was the bring-up size; Standard_D4s_v5 is the realistic
# size for the final benchmark runs (needs 12 vCPU of quota).
variable "node_size" {
  type    = string
  default = "Standard_D4s_v5"
}

# null = the AKS default GA version at apply time.
# Knative v1.22.1 is fine up to k8s 1.35.
variable "kubernetes_version" {
  type    = string
  default = null
}
