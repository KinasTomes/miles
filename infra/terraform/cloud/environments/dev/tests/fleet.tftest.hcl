mock_provider "aws" {
  mock_data "aws_ami" {
    defaults = { id = "ami-0123456789abcdef0", root_device_name = "/dev/xvda" }
  }
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001" }
  }
  mock_resource "aws_iam_service_linked_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling_miles-dev" }
  }
  mock_resource "aws_lb_target_group" {
    defaults = { arn = "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/miles-dev-s3/0123456789abcdef" }
  }
  mock_resource "aws_lb" {
    defaults = { arn = "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:loadbalancer/app/miles-dev/0123456789abcdef" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
}

variables {
  account_id                       = "123456789012"
  paused                           = false
  enable_asg                       = true
  enable_alb                       = true
  egress_mode                      = "interface"
  ami_id                           = "ami-0123456789abcdef0"
  fleet_instance_type              = "t4g.small"
  release_sha256                   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  release_version_id               = "fixture-version-1"
  runtime_permissions_boundary_arn = "arn:aws:iam::123456789012:policy/miles/bootstrap/MilesDevRuntimeBoundary"
  certificate_arn                  = "arn:aws:acm:ap-southeast-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
  client_cidrs                     = ["192.0.2.1/32"]
}

run "active_fleet" {
  command = apply # Mock provider only: no AWS resources or credentials.
  assert {
    condition     = output.fleet_contract.min_size == 2 && output.fleet_contract.desired == 2 && output.fleet_contract.max_size == 2 && length(output.fleet_contract.subnets) == 2 && output.fleet_contract.health_type == "ELB"
    error_message = "Cloud must attach a two-node/two-subnet ELB-health ASG."
  }
  assert {
    condition     = output.fleet_contract.heartbeat == 300 && output.fleet_contract.drain == 1800 && output.fleet_contract.controller_max > 1800 && output.alb_contract.idle_timeout == 120 && tonumber(output.alb_contract.drain_seconds) == 1800 && output.alb_contract.preserve_host_header
    error_message = "ALB/app/drain/lifecycle budgets must agree."
  }
  assert {
    condition     = output.runtime_inventory.compute.instance_id == null && output.runtime_inventory.compute.data_volume_id == null && length(output.runtime_inventory.expected_tables) == 20
    error_message = "Fleet uses shared data, never the retained single-node dev disk."
  }
}
run "paused_fleet_plan" {
  command = plan
  variables { paused = true }
  assert {
    condition     = output.fleet_contract == null && output.runtime_inventory.alb_dns == null && output.network_inventory.nat_count == 0 && length(output.network_inventory.interface_endpoints) == 0 && length(output.runtime_inventory.expected_tables) == 20
    error_message = "Pause removes fleet/ALB/paid API networking while retaining the datastore."
  }
}
run "paused_fleet" {
  command = apply
  variables { paused = true }
}
run "resumed_fleet_plan" {
  command = plan
  assert {
    condition     = output.fleet_contract.release_sha256 == run.active_fleet.fleet_contract.release_sha256 && output.runtime_inventory.data_bucket_name == run.active_fleet.runtime_inventory.data_bucket_name && output.runtime_inventory.kms_key_arn == run.active_fleet.runtime_inventory.kms_key_arn
    error_message = "Resume must preserve the pinned release and existing data/key identity."
  }
}
run "reject_unpinned_release" {
  command = plan
  variables { release_sha256 = null }
  expect_failures = [var.enable_asg]
}
run "reject_shared_dev_disk" {
  command = plan
  variables { enable_compute = true }
  expect_failures = [var.enable_asg]
}
run "reject_single_az_nat_dependency" {
  command = plan
  variables { egress_mode = "nat" }
  expect_failures = [var.enable_asg]
}
