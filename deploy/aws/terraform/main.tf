provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "netweather"
      ManagedBy = "terraform"
    }
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

# Ubuntu 24.04 LTS. The architecture follows ami_architecture so the same
# module works for t4g (arm64) and t3 (amd64) instance types.
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-${var.ami_architecture}-server-*"]
  }

  filter {
    name   = "architecture"
    values = [var.ami_architecture]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

resource "aws_vpc" "netweather" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = "netweather"
  }
}

resource "aws_internet_gateway" "netweather" {
  vpc_id = aws_vpc.netweather.id

  tags = {
    Name = "netweather"
  }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.netweather.id
  cidr_block              = var.subnet_cidr
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true

  tags = {
    Name = "netweather-public"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.netweather.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.netweather.id
  }

  tags = {
    Name = "netweather-public"
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "netweather" {
  name        = "netweather"
  description = "SSH and HTTP for the netweather k3s host"
  vpc_id      = aws_vpc.netweather.id

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

  ingress {
    description = "netweather HTTP through Traefik"
    from_port   = var.app_http_port
    to_port     = var.app_http_port
    protocol    = "tcp"
    cidr_blocks = [var.app_allowed_cidr]
  }

  egress {
    description = "Outbound updates, image pulls, and probes"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "netweather"
  }
}

resource "aws_instance" "netweather" {
  ami                         = data.aws_ami.ubuntu.id
  instance_type               = var.instance_type
  key_name                    = var.key_name
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.netweather.id]
  associate_public_ip_address = true

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_size
    delete_on_termination = true
    encrypted             = true
  }

  user_data = templatefile("${path.module}/user_data.yaml.tftpl", {
    netweather_image  = var.netweather_image
    manifest_base_url = var.manifest_base_url
  })

  tags = {
    Name = "netweather"
  }
}
