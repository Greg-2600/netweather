output "public_ip" {
  description = "Public IP address of the k3s host."
  value       = aws_instance.netweather.public_ip
}

output "public_dns" {
  description = "Public DNS name of the k3s host."
  value       = aws_instance.netweather.public_dns
}

output "app_url" {
  description = "HTTP URL for the netweather Ingress."
  value       = "http://${aws_instance.netweather.public_ip}:${var.app_http_port}"
}

output "ssh_command" {
  description = "SSH command for the host."
  value       = "ssh -i /path/to/${var.key_name}.pem ubuntu@${aws_instance.netweather.public_ip}"
}

output "kubeconfig_command" {
  description = "Command that copies the k3s kubeconfig to your machine."
  value       = "ssh -i /path/to/${var.key_name}.pem ubuntu@${aws_instance.netweather.public_ip} sudo cat /etc/rancher/k3s/k3s.yaml > netweather-kubeconfig.yaml"
}
