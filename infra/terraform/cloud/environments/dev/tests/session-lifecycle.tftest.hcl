# All apply/cleanup operations in this file use a mock provider: no AWS API calls.
mock_provider "aws" {
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::miles-dev-data-123456789012-ap-southeast-1", id = "miles-dev-data-123456789012-ap-southeast-1" } }
  mock_resource "aws_lb" { defaults = { arn = "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:loadbalancer/app/miles-dev/0000000000000001" } }
  mock_resource "aws_lb_target_group" { defaults = { arn = "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/miles-dev-s3/0000000000000001" } }
  mock_data "aws_ami" { defaults = { id = "ami-0123456789abcdef0" } }
  mock_resource "aws_kms_key" {
    defaults = {
      arn    = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001"
      key_id = "00000000-0000-0000-0000-000000000001"
    }
  }
}
variables {
  account_id                       = "123456789012"
  runtime_permissions_boundary_arn = "arn:aws:iam::123456789012:policy/miles/bootstrap/MilesDevRuntimeBoundary"
  enable_compute                   = true
  ami_id                           = "ami-0123456789abcdef0"
  egress_mode                      = "nat"
  enable_alb                       = true
  certificate_arn                  = "arn:aws:acm:ap-southeast-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
  client_cidrs                     = ["192.0.2.1/32"]
}
run "active_dev" {
  command = apply
  variables { paused = false }
  assert {
    condition     = output.network_inventory.nat_count == 1 && output.runtime_inventory.compute.instance_id != null && output.runtime_inventory.alb_dns != null
    error_message = "Active dev must contain the explicitly requested compute/NAT/ALB."
  }
}
run "pause_plan" {
  command = plan
  variables { paused = true }
}
run "pause_dev" {
  command = apply
  variables { paused = true }
  assert {
    condition     = output.network_inventory.nat_count == 0 && output.network_inventory.interface_endpoints == {} && output.runtime_inventory.compute.instance_id == null && output.runtime_inventory.alb_dns == null
    error_message = "Pause must remove the instance and all paid session networking."
  }
  assert {
    condition     = output.runtime_inventory.data == run.active_dev.runtime_inventory.data && output.runtime_inventory.kms_key_arn == run.active_dev.runtime_inventory.kms_key_arn && output.runtime_inventory.compute.data_volume_id == run.active_dev.runtime_inventory.compute.data_volume_id && output.network_inventory.vpc_id == run.active_dev.network_inventory.vpc_id && output.runtime_inventory.iam_roles == run.active_dev.runtime_inventory.iam_roles
    error_message = "Pause must preserve S3/DDB, data volume, KMS, VPC and runtime identities."
  }
}
run "resume_plan" {
  command = plan
  variables { paused = false }
}
run "resume_dev" {
  command = apply
  variables { paused = false }
  assert {
    condition     = output.network_inventory.nat_count == 1 && output.runtime_inventory.compute.instance_id != null && output.runtime_inventory.alb_dns != null && output.runtime_inventory.compute.data_volume_id == run.active_dev.runtime_inventory.compute.data_volume_id && output.runtime_inventory.data == run.active_dev.runtime_inventory.data
    error_message = "Resume must recreate session resources using the retained data resources."
  }
}
run "active_interfaces" {
  command = apply
  variables {
    paused      = false
    egress_mode = "interface"
  }
  assert {
    condition     = length(output.network_inventory.interface_endpoints) == 9 && output.network_inventory.nat_count == 0
    error_message = "Interface sessions need all nine services including ssmmessages."
  }
}
run "pause_interfaces" {
  command = plan
  variables {
    paused      = true
    egress_mode = "interface"
  }
  assert {
    condition     = output.network_inventory.interface_endpoints == {} && output.network_inventory.nat_count == 0 && output.runtime_inventory.compute.instance_id == null
    error_message = "Pause must also override desired interface egress."
  }
}
run "reject_active_compute_without_ami" {
  command = plan
  variables {
    paused = false
    ami_id = null
  }
  expect_failures = [var.enable_compute]
}
