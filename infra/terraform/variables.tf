variable "region" {
  type        = string
  description = "AWS region (MVP: ap-south-1, Mumbai)."
  default     = "ap-south-1"
}

variable "environment" {
  type        = string
  description = "Deployment environment tag."
  default     = "prod"
}

variable "project" {
  type    = string
  default = "safar-share"
}

variable "vpc_cidr" {
  type    = string
  default = "10.20.0.0/16"
}

variable "az_count" {
  type        = number
  description = "Number of AZs to spread public/private subnets across."
  default     = 2
}

# --- datastores ----------------------------------------------------------

variable "db_instance_class" {
  type    = string
  default = "db.t4g.medium" # 2 vCPU / 4 GiB — MVP floor for PostGIS
}

variable "db_name" {
  type    = string
  default = "safar"
}

variable "db_username" {
  type      = string
  default   = "safar"
  sensitive = true
}

variable "db_password" {
  type      = string
  sensitive = true
  description = "RDS master password (>=16 chars). Pass via TF_VAR_db_password."
}

variable "redis_node_type" {
  type    = string
  default = "cache.t4g.micro"
}

# --- API -----------------------------------------------------------------

variable "api_image" {
  type        = string
  description = "ECR image URI for the API (tagged with the git sha in CI)."
  default     = ""
}

variable "api_cpu" {
  type    = number
  default = 512
}

variable "api_memory" {
  type    = number
  default = 1024
}

variable "api_desired_count" {
  type    = number
  default = 2
}

variable "api_port" {
  type    = number
  default = 8080
}

# --- secrets (names in SSM; values set once out-of-band) -----------------

variable "ssm_jwt_secret_name" {
  type    = string
  default = "/safar-share/prod/jwt_secret"
}

variable "ssm_webhook_secret_name" {
  type    = string
  default = "/safar-share/prod/webhook_secret"
}

variable "allowed_origins" {
  type        = string
  description = "Comma-separated CORS allowlist (the web origin)."
  default     = "https://safar.example.com"
}
