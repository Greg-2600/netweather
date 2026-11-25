terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 7.0"
    }
  }

  # Remote state is optional. Create the bucket and lock table yourself, then
  # uncomment this block and fill in the values. No credentials belong here.
  #
  # backend "s3" {
  #   bucket         = "replace-with-your-state-bucket"
  #   key            = "netweather/terraform.tfstate"
  #   region         = "us-east-1"
  #   dynamodb_table = "replace-with-your-lock-table"
  #   encrypt        = true
  # }
}
