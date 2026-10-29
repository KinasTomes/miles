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
    aws_api_egress_configured = local.egress_mode != "isolated"
    public_subnet_cidrs       = module.network.public_subnet_cidrs
    gateway_endpoints         = module.endpoints.gateway_ids
    interface_endpoints       = module.endpoints.interface_ids
  }
}
output "runtime_inventory" {
  value = {
    iam_roles               = module.iam.role_arns
    instance_profiles       = module.iam.instance_profile_names
    data_bucket_name        = local.data_bucket_name
    expected_tables         = [for t in module.data.inventory.tables : t.name]
    data                    = module.data.inventory
    compute                 = module.compute.inventory
    kms_key_arn             = module.security.kms_key_arn
    secret_metadata_arn     = module.security.secret_arn
    bootstrap_parameter_arn = module.security.parameter_arn
    alb_dns                 = module.alb.dns_name
    target_group_arn        = module.alb.target_group_arn
  }
}
output "alb_contract" { value = module.alb.contract }
output "fleet_contract" { value = module.fleet.contract }
output "session_configuration" {
  description = "Retain the reviewed session settings while paused; resume must not change them."
  value = {
    enable_compute  = var.enable_compute
    ami_id          = var.ami_id
    egress_mode     = var.egress_mode
    nat_per_az      = var.nat_per_az
    enable_alb      = var.enable_alb
    certificate_arn = var.certificate_arn
  }
}
