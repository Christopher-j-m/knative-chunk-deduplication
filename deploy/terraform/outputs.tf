output "acr_login_server" {
  value = azurerm_container_registry.corpus.login_server
}

output "resource_group" {
  value = azurerm_resource_group.thesis.name
}

output "cluster_name" {
  value = azurerm_kubernetes_cluster.cache.name
}

output "kubeconfig_command" {
  value = "az aks get-credentials --resource-group ${azurerm_resource_group.thesis.name} --name ${azurerm_kubernetes_cluster.cache.name}"
}
