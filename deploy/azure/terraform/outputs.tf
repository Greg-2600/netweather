output "public_ip" {
  description = "Public IP address of the k3s host."
  value       = azurerm_public_ip.netweather.ip_address
}

output "app_url" {
  description = "HTTPS URL for the netweather Ingress."
  value       = "https://${var.ingress_host}"
}

output "public_ip_url" {
  description = "HTTP URL for the public IP address, useful before DNS is configured."
  value       = "http://${azurerm_public_ip.netweather.ip_address}"
}

output "ssh_command" {
  description = "SSH command for the host. Replace the key path with your private key."
  value       = "ssh -i /path/to/private_key ${var.admin_username}@${azurerm_public_ip.netweather.ip_address}"
}

output "kubeconfig_command" {
  description = "Command that copies the k3s kubeconfig to your machine."
  value       = "ssh -i /path/to/private_key ${var.admin_username}@${azurerm_public_ip.netweather.ip_address} sudo cat /etc/rancher/k3s/k3s.yaml > netweather-kubeconfig.yaml"
}

output "resource_group_name" {
  description = "Resource group that holds the deployment."
  value       = azurerm_resource_group.netweather.name
}

output "agent_private_ips" {
  description = "Private IP addresses of the k3s agent nodes."
  value       = azurerm_network_interface.agent[*].private_ip_address
}
