variable "location" {
  description = "Azure region for the k3s host."
  type        = string
  default     = "eastus"
}

variable "resource_group_name" {
  description = "Name of the resource group created for this deployment."
  type        = string
  default     = "netweather-rg"
}

variable "vm_size" {
  description = "Azure VM size. Standard_B1s is the low cost default. Standard_B2ats_v2 is a larger alternative with more memory."
  type        = string
  default     = "Standard_B1s"
}

variable "admin_username" {
  description = "Linux user created on the VM for SSH access."
  type        = string
  default     = "azureuser"

  validation {
    condition     = !contains(["admin", "root", "test", "user", "administrator"], lower(var.admin_username))
    error_message = "admin_username must not be a reserved name."
  }
}

variable "ssh_public_key" {
  description = "SSH public key for admin_username. Pass the contents of your .pub file, not a private key."
  type        = string
}

variable "ssh_allowed_cidr" {
  description = "CIDR block allowed to reach SSH. Use your own address, for example 203.0.113.10/32."
  type        = string
}

variable "app_allowed_cidr" {
  description = "Source allowed to reach the netweather HTTP port."
  type        = string
  default     = "*"
}

variable "app_http_port" {
  description = "Public HTTP port served by the k3s Traefik Ingress."
  type        = number
  default     = 80
}

variable "vnet_cidr" {
  description = "CIDR block for the virtual network created for this deployment."
  type        = string
  default     = "10.42.0.0/16"
}

variable "subnet_cidr" {
  description = "CIDR block for the single subnet."
  type        = string
  default     = "10.42.1.0/24"
}

variable "public_ip_sku" {
  description = "Public IP SKU. Basic is the low cost choice where the subscription and region still support it. Standard is the fallback for newer subscriptions."
  type        = string
  default     = "Basic"

  validation {
    condition     = contains(["Basic", "Standard"], var.public_ip_sku)
    error_message = "public_ip_sku must be Basic or Standard."
  }
}

variable "root_disk_size_gb" {
  description = "Root disk size in GiB."
  type        = number
  default     = 30
}

variable "auto_shutdown_enabled" {
  description = "Stop the VM on a daily schedule to reduce cost. The default keeps the service running."
  type        = bool
  default     = false
}

variable "auto_shutdown_time" {
  description = "Daily shutdown time in HHMM format when auto shutdown is enabled."
  type        = string
  default     = "1900"
}

variable "auto_shutdown_timezone" {
  description = "Time zone for the auto shutdown schedule."
  type        = string
  default     = "UTC"
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

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default = {
    Project   = "netweather"
    ManagedBy = "terraform"
  }
}
