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

variable "acr_name" {
  type    = string
}

variable "node_count" {
  type    = number
  default = 3
}

variable "node_size" {
  type    = string
  default = "Standard_D4s_v5"
}

variable "kubernetes_version" {
  type    = string
  default = null
}
