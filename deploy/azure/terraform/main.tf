provider "azurerm" {
  features {}
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
}

resource "azurerm_subnet" "netweather" {
  name                 = "netweather-subnet"
  resource_group_name  = azurerm_resource_group.netweather.name
  virtual_network_name = azurerm_virtual_network.netweather.name
  address_prefixes     = [var.subnet_cidr]
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
}

resource "azurerm_public_ip" "netweather" {
  name                = "netweather-public-ip"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  allocation_method   = "Static"
  sku                 = var.public_ip_sku
  tags                = var.tags
}

resource "azurerm_network_interface" "netweather" {
  name                = "netweather-nic"
  location            = azurerm_resource_group.netweather.location
  resource_group_name = azurerm_resource_group.netweather.name
  tags                = var.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.netweather.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.netweather.id
  }
}

resource "azurerm_network_interface_security_group_association" "netweather" {
  network_interface_id      = azurerm_network_interface.netweather.id
  network_security_group_id = azurerm_network_security_group.netweather.id
}

resource "azurerm_linux_virtual_machine" "netweather" {
  name                            = "netweather"
  location                        = azurerm_resource_group.netweather.location
  resource_group_name             = azurerm_resource_group.netweather.name
  size                            = var.vm_size
  admin_username                  = var.admin_username
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.netweather.id]
  tags                            = var.tags

  admin_ssh_key {
    username   = var.admin_username
    public_key = var.ssh_public_key
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"
    disk_size_gb         = var.root_disk_size_gb
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  custom_data = base64encode(templatefile("${path.module}/cloud_init.yaml.tftpl", {
    netweather_image  = var.netweather_image
    manifest_base_url = var.manifest_base_url
  }))
}

resource "azurerm_dev_test_global_vm_shutdown_schedule" "netweather" {
  count = var.auto_shutdown_enabled ? 1 : 0

  virtual_machine_id    = azurerm_linux_virtual_machine.netweather.id
  location              = azurerm_resource_group.netweather.location
  enabled               = true
  daily_recurrence_time = var.auto_shutdown_time
  timezone              = var.auto_shutdown_timezone

  notification_settings {
    enabled = false
  }

  tags = var.tags
}
