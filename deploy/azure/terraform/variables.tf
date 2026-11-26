variable "location" {
  description = "Azure region for the k3s hosts."
  type        = string
  default     = "northcentralus"
}

variable "resource_group_name" {
  description = "Name of the resource group created for this deployment."
  type        = string
  default     = "netweather-rg"
}

variable "server_vm_size" {
  description = "Azure VM size for the k3s server. Standard_B2ats_v2 is an AMD size covered by the student free allowance."
  type        = string
  default     = "Standard_B2ats_v2"
}

variable "agent_vm_size" {
  description = "Azure VM size for the k3s agent. Standard_B2pts_v2 is an Arm size covered by the student free allowance."
  type        = string
  default     = "Standard_B2pts_v2"
}

variable "agent_count" {
  description = "Number of k3s agent nodes. Set to 0 for a single server node."
  type        = number
  default     = 1

  validation {
    condition     = var.agent_count >= 0
    error_message = "agent_count must be zero or greater."
  }
}

variable "app_node_arch" {
  description = "CPU architecture used in the netweather nodeSelector. The default arm64 matches the Arm agent. Set to an empty string to omit the nodeSelector."
  type        = string
  default     = "arm64"
}

variable "admin_username" {
  description = "Linux user created on the VMs for SSH access."
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
  description = "Source allowed to reach the netweather HTTP and HTTPS ports."
  type        = string
  default     = "*"
}

variable "app_http_port" {
  description = "Public HTTP port served by the k3s Traefik Ingress."
  type        = number
  default     = 80
}

variable "app_https_port" {
  description = "Public HTTPS port served by the k3s Traefik Ingress."
  type        = number
  default     = 443
}

variable "ingress_host" {
  description = "Host name served by the Traefik Ingress."
  type        = string
  default     = "netweather.westcountytech.com"
}

variable "acme_email" {
  description = "Email address used for the Let's Encrypt ACME account."
  type        = string
  default     = "greg@westcountytech.com"
}

variable "vnet_cidr" {
  description = "CIDR block for the virtual network created for this deployment. It must not overlap k3s_cluster_cidr or k3s_service_cidr."
  type        = string
  default     = "10.240.0.0/16"
}

variable "subnet_cidr" {
  description = "CIDR block for the single subnet. It must be inside vnet_cidr and must not overlap k3s_cluster_cidr or k3s_service_cidr."
  type        = string
  default     = "10.240.1.0/24"
}

variable "server_private_ip" {
  description = "Static private IP address assigned to the k3s server NIC. Agents use this address to join the cluster. It must be inside subnet_cidr and clear of the k3s pod and service CIDRs."
  type        = string
  default     = "10.240.1.4"
}

variable "k3s_cluster_cidr" {
  description = "CIDR block used for k3s pod addresses. It must not overlap vnet_cidr or subnet_cidr."
  type        = string
  default     = "10.42.0.0/16"
}

variable "k3s_service_cidr" {
  description = "CIDR block used for k3s service addresses. It must not overlap vnet_cidr or subnet_cidr."
  type        = string
  default     = "10.43.0.0/16"
}

variable "public_ip_sku" {
  description = "Public IP SKU. Standard is required for a static IPv4 address on newer subscriptions."
  type        = string
  default     = "Standard"

  validation {
    condition     = contains(["Basic", "Standard"], var.public_ip_sku)
    error_message = "public_ip_sku must be Basic or Standard."
  }
}

variable "root_disk_size_gb" {
  description = "Root disk size in GiB. The default P6 disk is covered by the student free allowance."
  type        = number
  default     = 64
}

variable "os_disk_storage_account_type" {
  description = "Storage account type for the OS disks."
  type        = string
  default     = "Premium_LRS"

  validation {
    condition     = contains(["Premium_LRS", "StandardSSD_LRS", "Standard_LRS"], var.os_disk_storage_account_type)
    error_message = "os_disk_storage_account_type must be Premium_LRS, StandardSSD_LRS, or Standard_LRS."
  }
}

variable "server_image_sku" {
  description = "Ubuntu 24.04 image SKU for the server. The default server SKU is x86_64."
  type        = string
  default     = "server"
}

variable "agent_image_sku" {
  description = "Ubuntu 24.04 image SKU for the agents. The default server-arm64 SKU matches the Arm B2pts_v2 size."
  type        = string
  default     = "server-arm64"
}

variable "swap_size_mb" {
  description = "Swap file size in MiB created on every node. Set to 0 to skip the swap file."
  type        = number
  default     = 2048
}

variable "auto_shutdown_enabled" {
  description = "Stop the VMs on a daily schedule to reduce cost. The default keeps the service running."
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
  description = "GHCR image built from this repository, including the tag. The image is multi arch."
  type        = string
  default     = "ghcr.io/greg-2600/netweather:1.0.1"
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
