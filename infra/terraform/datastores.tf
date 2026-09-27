# Datastores: RDS PostgreSQL 16 + PostGIS 3.4, ElastiCache Redis 7.

resource "random_password" "db" {
  length  = 24
  special = false # avoid URL-encoding surprises in DATABASE_URL
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.project}-db"
  subnet_ids = aws_subnet.private[*].id
  tags       = { Name = "${var.project}-db-subnets" }
}

resource "aws_db_parameter_group" "this" {
  name   = "${var.project}-pg16"
  family = "postgres16"

  # PostGIS is a supported extension on RDS PG16; the migration runs
  # CREATE EXTENSION IF NOT EXISTS postgis on first boot (001_init.sql).
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  parameter {
    name  = "log_min_duration_statement"
    value = "500" # log queries > 500 ms
  }
}

resource "aws_db_instance" "this" {
  identifier     = "${var.project}-db"
  engine         = "postgres"
  engine_version = "16.3"
  instance_class = var.db_instance_class

  allocated_storage     = 50
  max_allocated_storage = 200
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = var.db_name
  username = var.db_username
  password = random_password.db.result

  db_subnet_group_name   = aws_db_subnet_group.this.name
  parameter_group_name   = aws_db_parameter_group.this.name
  vpc_security_group_ids = [aws_security_group.datastores.id]

  backup_retention_period = 7
  backup_window           = "03:00-04:00"
  maintenance_window      = "sun:04:00-sun:05:00"
  skip_final_snapshot     = var.environment != "prod"
  deletion_protection     = var.environment == "prod"

  multi_az            = var.environment == "prod"
  publicly_accessible = false

  tags = { Name = "${var.project}-db" }
}

resource "aws_elasticache_subnet_group" "this" {
  name       = "${var.project}-redis"
  subnet_ids = aws_subnet.private[*].id
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.project}-redis"
  description          = "Safar Share session cache, OTPs, rate limits, seat locks"
  engine               = "redis"
  engine_version       = "7.1"
  node_type            = var.redis_node_type
  num_cache_clusters   = 2 # primary + replica, automatic failover
  parameter_group_name = "default.redis7"
  port                 = 6379

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = [aws_security_group.datastores.id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = true
  automatic_failover_enabled = true
  multi_az_enabled           = var.environment == "prod"

  tags = { Name = "${var.project}-redis" }
}
