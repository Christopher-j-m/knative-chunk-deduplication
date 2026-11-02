resource "azurerm_resource_group" "thesis" {
  name     = var.resource_group_name
  location = var.location
}

resource "azurerm_container_registry" "corpus" {
  name                   = var.acr_name
  resource_group_name    = azurerm_resource_group.thesis.name
  location               = azurerm_resource_group.thesis.location
  sku                    = "Standard"
  admin_enabled          = false
  anonymous_pull_enabled = true
}

resource "azurerm_kubernetes_cluster" "cache" {
  name                = var.cluster_name
  location            = azurerm_resource_group.thesis.location
  resource_group_name = azurerm_resource_group.thesis.name
  dns_prefix          = var.cluster_name
  kubernetes_version  = var.kubernetes_version
  sku_tier            = "Free"

  // Disable auto-updating, since that would break local changes in the nydus snapshotter
  node_os_upgrade_channel = "None"
  
  oidc_issuer_enabled = false

  node_provisioning_profile {
    mode = "Manual"
  }

  default_node_pool {
    name       = "rotatepool"
    node_count = var.node_count
    vm_size    = var.node_size
    temporary_name_for_rotation = "nodepool"
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    service_cidr        = "10.0.0.0/16"
    dns_service_ip      = "10.0.0.10"
  }
}

# Use kubelets managed identity to allow them pulling from the acr => so we dont need anonymous pulling 
resource "azurerm_role_assignment" "acr_pull" {
  scope                = azurerm_container_registry.corpus.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.cache.kubelet_identity[0].object_id
}
