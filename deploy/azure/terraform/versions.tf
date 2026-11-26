terraform {
  required_version = ">= 1.5.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 4.0, < 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.0"
    }
  }

  # Remote state is optional. Create a storage account and container yourself,
  # then uncomment this block and fill in the values. No credentials belong here.
  #
  # backend "azurerm" {
  #   resource_group_name  = "replace-with-your-state-resource-group"
  #   storage_account_name = "replacewithyourstateaccount"
  #   container_name       = "tfstate"
  #   key                  = "netweather/terraform.tfstate"
  # }
}
