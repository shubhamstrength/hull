# Deploys an image built and pushed with hull to AWS ECS Fargate.
#
#   1. terraform init && terraform apply -target=aws_ecr_repository.app
#   2. aws ecr get-login-password | hull login -u AWS --password-stdin <registry from step 1>
#      hull push --digest-file image.txt      (with image: set to the repository URL in hull.yaml)
#   3. terraform apply -var "image=$(cat image.txt)"

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "name" {
  type    = string
  default = "react-app"
}

variable "image" {
  description = "Image reference printed by `hull push`, e.g. <repo>@sha256:..."
  type        = string
  default     = ""
}

resource "aws_ecr_repository" "app" {
  name                 = var.name
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

# The account's default VPC keeps the example short; use your own VPC in production.
data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}

module "app" {
  source = "../../modules/aws-ecs-fargate"
  count  = var.image == "" ? 0 : 1

  name           = var.name
  image          = var.image
  container_port = 8080
  vpc_id         = data.aws_vpc.default.id
  subnet_ids     = data.aws_subnets.default.ids
}

output "repository_url" {
  description = "Set this as image: in hull.yaml."
  value       = aws_ecr_repository.app.repository_url
}

output "url" {
  value = try(module.app[0].url, "push an image and apply again with -var image=...")
}
