resource "azurerm_resource_group" "thesis" {
  name     = var.resource_group_name
  location = var.location
}

# Standard tier: the cheapest tier that supports anonymous pull, which the
# chunk-proxy's auth-less registry client requires.
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

  # A node reimage would silently revert the containerd config and the
  # host-installed snapshotter, so cluster auto-upgrade stays off (the
  # provider default when the channel is unset) and node OS reimaging is None
  node_os_upgrade_channel = "None"

  default_node_pool {
    # The pool kept the rotation name after the D4s_v5 resize: swapping the
    # name back would need both pools alive at once, and two D4s_v5 pools
    # exceed the regional vCPU quota. The name is cosmetic, so it stays
    name       = "rotatepool"
    node_count = var.node_count
    vm_size    = var.node_size

    # Azure swaps the pool through a temporary one when the VM size
    # changes, so a resize does not have to tear down the whole cluster
    temporary_name_for_rotation = "nodepool"
  }

  identity {
    type = "SystemAssigned"
  }

  # Service CIDR pinned so the cache-service ClusterIP 10.0.0.50 is valid.
  network_profile {
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    service_cidr        = "10.0.0.0/16"
    dns_service_ip      = "10.0.0.10"
  }
}

# Lets the kubelets pull from ACR with their managed identity, so CRI image
# pulls never depend on the anonymous-pull flag.
resource "azurerm_role_assignment" "acr_pull" {
  scope                = azurerm_container_registry.corpus.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.cache.kubelet_identity[0].object_id
}
