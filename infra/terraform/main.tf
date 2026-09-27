# Safar Share — Terraform (§14): AWS primitives for the MVP.
#   terraform init && terraform plan -var-file=prod.tfvars
# Scope: network, RDS (PostGIS), ElastiCache (Redis), ECS Fargate API,
# S3+CloudFront web, secrets in SSM. No cluster vendor lock beyond AWS.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # Remote state — bucket/dynamo created once by hand or in bootstrap.
  backend "s3" {
    bucket         = "safar-share-tfstate"
    key            = "mvp/terraform.tfstate"
    region         = "ap-south-1"
    dynamodb_table = "safar-share-tf-locks"
    encrypt        = true
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "safar-share"
      Env       = var.environment
      ManagedBy = "terraform"
    }
  }
}
