provider "azurerm" {
  features {}
}

resource "random_password" "k3s_token" {
  length  = 48
  special = false
}

resource "azurerm_resource_group" "netweather" {
  name     = var.resource_group_name
  location = var.location
  tags     = var.tags
}

resource "azurerm_virtual_network" "netweather" {
  name                = "netweather-vnet"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  address_space       = [var.vnet_cidr]
  tags                = var.tags

  lifecycle {
    precondition {
      condition = (
        split(".", var.vnet_cidr)[0] != split(".", var.k3s_cluster_cidr)[0] ||
        split(".", var.vnet_cidr)[1] != split(".", var.k3s_cluster_cidr)[1]
        ) && (
        split(".", var.vnet_cidr)[0] != split(".", var.k3s_service_cidr)[0] ||
        split(".", var.vnet_cidr)[1] != split(".", var.k3s_service_cidr)[1]
      )
      error_message = "vnet_cidr must not overlap k3s_cluster_cidr or k3s_service_cidr. Change vnet_cidr, subnet_cidr, or the k3s CIDRs so the node network stays clear of the pod and service networks."
    }
  }
}

resource "azurerm_subnet" "netweather" {
  name                 = "netweather-subnet"
  resource_group_name  = azurerm_resource_group.netweather.name
  virtual_network_name = azurerm_virtual_network.netweather.name
  address_prefixes     = [var.subnet_cidr]

  # Agents have no public IP. Default outbound access lets them reach the
  # internet without a NAT gateway or load balancer.
  default_outbound_access_enabled = true
}

resource "azurerm_network_security_group" "netweather" {
  name                = "netweather-nsg"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  tags                = var.tags

  security_rule {
    name                       = "SSH"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = var.ssh_allowed_cidr
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "NetweatherHTTP"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = tostring(var.app_http_port)
    source_address_prefix      = var.app_allowed_cidr
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "NetweatherHTTPS"
    priority                   = 120
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = tostring(var.app_https_port)
    source_address_prefix      = var.app_allowed_cidr
    destination_address_prefix = "*"
  }
}

resource "azurerm_public_ip" "netweather" {
  name                = "netweather-public-ip"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  allocation_method   = "Static"
  sku                 = var.public_ip_sku
  tags                = var.tags
}

resource "azurerm_network_interface" "server" {
  name                = "netweather-server-nic"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  tags                = var.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.netweather.id
    private_ip_address_allocation = "Static"
    private_ip_address            = var.server_private_ip
    public_ip_address_id          = azurerm_public_ip.netweather.id
  }
}

resource "azurerm_network_interface_security_group_association" "server" {
  network_interface_id      = azurerm_network_interface.server.id
  network_security_group_id = azurerm_network_security_group.netweather.id
}

resource "azurerm_linux_virtual_machine" "server" {
  name                            = "netweather"
  location                        = azurerm_resource_group.netweather.location
  resource_group_name             = azurerm_resource_group.netweather.name
  size                            = var.server_vm_size
  admin_username                  = var.admin_username
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.server.id]
  tags                            = var.tags

  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.os_disk_storage_account_type
    disk_size_gb         = var.root_disk_size_gb
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = var.server_image_sku
    version   = "latest"
  }

  custom_data = base64encode(templatefile("${path.module}/cloud_init.yaml.tftpl", {
    netweather_image  = var.netweather_image
    manifest_base_url = var.manifest_base_url
    ingress_host      = var.ingress_host
    acme_email        = var.acme_email
    k3s_token         = random_password.k3s_token.result
    server_private_ip = var.server_private_ip
    k3s_cluster_cidr  = var.k3s_cluster_cidr
    k3s_service_cidr  = var.k3s_service_cidr
    swap_size_mb      = var.swap_size_mb
    app_node_arch     = var.app_node_arch
    app_on_agent      = var.app_node_arch != "" && var.agent_count > 0
  }))
}

resource "azurerm_network_interface" "agent" {
  count = var.agent_count

  # Dynamic agent IPs must not grab the address reserved for the static server NIC.
  depends_on = [azurerm_network_interface.server]

  name                = "netweather-agent-${count.index + 1}-nic"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  tags                = var.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.netweather.id
    private_ip_address_allocation = "Dynamic"
  }
}

resource "azurerm_network_interface_security_group_association" "agent" {
  count = var.agent_count

  network_interface_id      = azurerm_network_interface.agent[count.index].id
  network_security_group_id = azurerm_network_security_group.netweather.id
}

resource "azurerm_linux_virtual_machine" "agent" {
  count = var.agent_count

  name                            = "netweather-agent-${count.index + 1}"
  location                        = azurerm_resource_group.netweather.location
  resource_group_name             = azurerm_resource_group.netweather.name
  size                            = var.agent_vm_size
  admin_username                  = var.admin_username
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.agent[count.index].id]
  tags                            = var.tags

  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.os_disk_storage_account_type
    disk_size_gb         = var.root_disk_size_gb
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = var.agent_image_sku
    version   = "latest"
  }

  custom_data = base64encode(templatefile("${path.module}/cloud_init_agent.yaml.tftpl", {
    k3s_token         = random_password.k3s_token.result
    server_private_ip = var.server_private_ip
    swap_size_mb      = var.swap_size_mb
  }))
}

locals {
  vm_ids = merge(
    { server = azurerm_linux_virtual_machine.server.id },
    { for index, vm in azurerm_linux_virtual_machine.agent : "agent-${index + 1}" => vm.id }
  )
}

resource "azurerm_dev_test_global_vm_shutdown_schedule" "netweather" {
  for_each = var.auto_shutdown_enabled ? local.vm_ids : {}

  virtual_machine_id    = each.value
  location              = azurerm_resource_group.netweather.location
  enabled               = true
  daily_recurrence_time = var.auto_shutdown_time
  timezone              = var.auto_shutdown_timezone

  notification_settings {
    enabled = false
  }

  tags = var.tags
}
