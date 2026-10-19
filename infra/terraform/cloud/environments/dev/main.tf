locals {
  name             = "miles-dev"
  data_bucket_name = "miles-dev-data-${var.account_id}-${var.region}"
  # Names are the approved schema; this task does not provision tables or runtime data bucket.
  table_names = toset(["Users", "Buckets", "BucketGrants", "AdminTokens", "Credentials", "SystemConfig", "Objects", "ManifestPages", "Chunks", "Packs", "Uploads", "UploadParts", "UploadPartPages", "CompletionPages", "Outbox", "RecoveryEpochs", "QuotaReservations", "Leases", "Jobs", "Nodes"])
}
module "network" {
  source          = "../../modules/network"
  name            = local.name
  cidr            = var.vpc_cidr
  zones           = var.availability_zones
  egress_mode     = var.egress_mode
  nat_per_az      = var.nat_per_az
  client_cidrs    = var.client_cidrs
  admin_ssh_cidrs = var.admin_ssh_cidrs
}
module "endpoints" {
  source            = "../../modules/endpoints"
  name              = local.name
  region            = var.region
  vpc_id            = module.network.vpc_id
  private_subnets   = module.network.private_subnets
  route_table_ids   = module.network.private_route_table_ids
  gateway_sg_id     = module.network.security_groups.gateway
  jobs_sg_id        = module.network.security_groups.jobs
  enable_interfaces = var.egress_mode == "interface"
  data_bucket_name  = local.data_bucket_name
  table_arns        = [for n in local.table_names : "arn:aws:dynamodb:${var.region}:${var.account_id}:table/${local.name}-${n}"]
}
module "security" {
  source     = "../../modules/security"
  name       = local.name
  region     = var.region
  account_id = var.account_id
}
module "iam" {
  source           = "../../modules/iam"
  name             = local.name
  region           = var.region
  account_id       = var.account_id
  data_bucket_name = local.data_bucket_name
  table_arns       = [for n in local.table_names : "arn:aws:dynamodb:${var.region}:${var.account_id}:table/${local.name}-${n}"]
  kms_key_arn      = module.security.kms_key_arn
  secret_arn       = module.security.secret_arn
  parameter_arn    = module.security.parameter_arn
  log_group_arn    = module.security.log_group_arn
}
module "alb" {
  source            = "../../modules/alb"
  name              = local.name
  enabled           = var.enable_alb
  vpc_id            = module.network.vpc_id
  subnet_ids        = values(module.network.public_subnets)
  security_group_id = module.network.security_groups.alb
  certificate_arn   = var.certificate_arn
  client_cidrs      = var.client_cidrs
}
