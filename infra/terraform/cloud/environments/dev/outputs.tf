output "network_inventory" {
  description = "Configured network paths only; aws_api_egress_configured is not a workload readiness probe."
  value = {
    vpc_id                    = module.network.vpc_id
    public_subnets            = module.network.public_subnets
    private_subnets           = module.network.private_subnets
    security_groups           = module.network.security_groups
    paused                    = var.paused
    egress_mode               = local.egress_mode
    nat_count                 = module.network.nat_count
    nat_gateway_ids           = module.network.nat_gateway_ids
    elastic_ip_allocation_ids = module.network.elastic_ip_allocation_ids
    aws_api_egress_configured = local.egress_mode != "isolated"
    public_subnet_cidrs       = module.network.public_subnet_cidrs
    gateway_endpoints         = module.endpoints.gateway_ids
    interface_endpoints       = module.endpoints.interface_ids
  }
}
output "runtime_inventory" {
  value = {
    iam_roles                 = module.iam.role_arns
    edge                      = try(module.edge[0].inventory, null)
    instance_profiles         = module.iam.instance_profile_names
    data_bucket_name          = local.data_bucket_name
    expected_tables           = [for t in module.data.inventory.tables : t.name]
    data                      = module.data.inventory
    compute                   = module.compute.inventory
    kms_key_arn               = module.security.kms_key_arn
    secret_metadata_arn       = module.security.secret_arn
    edge_bootstrap_secret_arn = try(aws_secretsmanager_secret.edge_bootstrap[0].arn, null)
    bootstrap_parameter_arn   = module.security.parameter_arn
    alb_dns                   = module.alb.dns_name
    target_group_arn          = module.alb.target_group_arn
    alb_name                  = module.alb.alb_name
    alb_arn                   = module.alb.alb_arn
    telemetry_document        = module.acceptance.telemetry_document
  }
}
output "alb_contract" { value = module.alb.contract }
output "fleet_contract" { value = module.fleet.contract }
output "edge_provider" { value = module.iam.edge_provider }
output "session_configuration" {
  description = "Retain the reviewed session settings while paused; resume must not change them."
  value = {
    installation_name                = var.installation_name
    region                           = var.region
    environment                      = var.environment
    owner                            = var.owner
    runtime_iam_path                 = var.runtime_iam_path
    runtime_permissions_boundary_arn = var.runtime_permissions_boundary_arn
    edge_certificate_common_name     = var.edge_certificate_common_name
    instance_type                    = var.instance_type
    release_architecture             = var.release_architecture
    enable_asg                       = var.enable_asg
    enable_edge                      = var.enable_edge
    edge_instance_type               = var.edge_instance_type
    edge_helper_sha256               = var.edge_helper_sha256
    edge_helper_version_id           = var.edge_helper_version_id
    fleet_instance_type              = var.fleet_instance_type
    release_sha256                   = var.release_sha256
    release_version_id               = var.release_version_id
    enable_compute                   = var.enable_compute
    ami_id                           = var.ami_id
    egress_mode                      = var.egress_mode
    nat_per_az                       = var.nat_per_az
    enable_alb                       = var.enable_alb
    certificate_arn                  = var.certificate_arn
  }
}
output "alarm_contract" { value = module.observability.contract }
