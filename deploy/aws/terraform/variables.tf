variable "region" {
  description = "AWS region for the k3s host."
  type        = string
  default     = "us-east-1"
}

variable "instance_type" {
  description = "EC2 instance type. t4g.small is arm64. Use t3.micro with ami_architecture set to amd64."
  type        = string
  default     = "t4g.small"
}

variable "ami_architecture" {
  description = "Ubuntu AMI architecture. Use arm64 for t4g instances and amd64 for t3 instances."
  type        = string
  default     = "arm64"

  validation {
    condition     = contains(["arm64", "amd64"], var.ami_architecture)
    error_message = "ami_architecture must be arm64 or amd64."
  }
}

variable "key_name" {
  description = "Name of an existing EC2 key pair for SSH access."
  type        = string
}

variable "ssh_allowed_cidr" {
  description = "CIDR block allowed to reach SSH. Use your own address, for example 203.0.113.10/32."
  type        = string
}

variable "app_allowed_cidr" {
  description = "CIDR block allowed to reach the netweather Ingress."
  type        = string
  default     = "0.0.0.0/0"
}

variable "app_http_port" {
  description = "Public HTTP port served by the k3s Traefik Ingress."
  type        = number
  default     = 80
}

variable "vpc_cidr" {
  description = "CIDR block for the small VPC created for this deployment."
  type        = string
  default     = "10.42.0.0/16"
}

variable "subnet_cidr" {
  description = "CIDR block for the single public subnet."
  type        = string
  default     = "10.42.1.0/24"
}

variable "root_volume_size" {
  description = "Root EBS volume size in GiB."
  type        = number
  default     = 8
}

variable "netweather_image" {
  description = "GHCR image built from this repository, including the tag."
  type        = string
  default     = "ghcr.io/greg-2600/netweather:latest"
}

variable "manifest_base_url" {
  description = "Base URL for the Kubernetes manifests applied by cloud init."
  type        = string
  default     = "https://raw.githubusercontent.com/Greg-2600/netweather/main/deploy/k8s"
}
