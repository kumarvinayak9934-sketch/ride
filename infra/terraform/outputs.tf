output "vpc_id" {
  value = aws_vpc.this.id
}

output "api_alb_dns" {
  description = "Public entry point for the API (point the API subdomain here)."
  value       = aws_lb.api.dns_name
}

output "api_ecr_repository" {
  description = "Push target for CI-built API images."
  value       = aws_ecr_repository.api.repository_url
}

output "db_endpoint" {
  value = aws_db_instance.this.address
}

output "redis_endpoint" {
  value = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "web_bucket" {
  value = aws_s3_bucket.web.id
}

output "web_cdn_domain" {
  description = "CloudFront domain serving the exported web app."
  value       = aws_cloudfront_distribution.web.domain_name
}

output "database_url_ssm_hint" {
  description = "Compose DATABASE_URL out of these + random_password.db (sensitive, shown in console)."
  value       = "postgres://${var.db_username}@${aws_db_instance.this.address}:5432/${var.db_name}?sslmode=require"
}
