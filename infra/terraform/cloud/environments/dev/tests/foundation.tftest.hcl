mock_provider "aws" {}

# AWS documentation example account, never a live deployment target.
variables { account_id = "123456789012" }

run "isolated_foundation" {
  command = plan
  assert {
    condition     = length(output.network_inventory.public_subnets) == 2 && length(output.network_inventory.private_subnets) == 2 && length(output.network_inventory.gateway_endpoints) == 2
    error_message = "Foundation needs four subnets and S3/DDB gateway endpoints."
  }
  assert {
    condition     = !output.network_inventory.aws_api_egress_configured && output.network_inventory.nat_count == 0 && length(output.network_inventory.interface_endpoints) == 0 && output.runtime_inventory.alb_dns == null
    error_message = "Default foundation must not silently create paid egress/ALB or claim readiness."
  }
}
run "single_nat_dev" {
  command = plan
  variables { egress_mode = "nat" }
  assert {
    condition     = output.network_inventory.nat_count == 1
    error_message = "Single NAT dev mode must create one NAT."
  }
}
run "nat_per_az" {
  command = plan
  variables {
    egress_mode = "nat"
    nat_per_az  = true
  }
  assert {
    condition     = output.network_inventory.nat_count == 2
    error_message = "Per-AZ NAT mode needs two NATs."
  }
}
run "private_aws_apis" {
  command = plan
  variables { egress_mode = "interface" }
  assert {
    condition     = length(output.network_inventory.interface_endpoints) == 8 && output.network_inventory.nat_count == 0
    error_message = "Interface mode must cover eight AWS APIs without NAT."
  }
}
run "reject_alb_without_tls" {
  command = plan
  variables { enable_alb = true }
  expect_failures = [var.enable_alb]
}
run "https_alb_plan" {
  command = plan
  variables {
    enable_alb      = true
    certificate_arn = "arn:aws:acm:ap-southeast-1:123456789012:certificate/00000000-0000-0000-0000-000000000000"
    client_cidrs    = ["192.0.2.1/32"]
  }
  assert {
    condition     = output.alb_contract.preserve_host_header && output.alb_contract.idle_timeout == 120 && output.alb_contract.listener_protocol == "HTTPS" && output.alb_contract.listener_port == 443 && output.alb_contract.target_port == 8080 && output.alb_contract.health_port == "9100" && output.alb_contract.health_path == "/readyz" && tonumber(output.alb_contract.drain_seconds) == 1800
    error_message = "ALB with test certificate should plan its target group."
  }
}

run "reject_small_alb_subnets" {
  command = plan
  variables { vpc_cidr = "10.0.0.0/20" }
  expect_failures = [var.vpc_cidr]
}
run "smallest_supported_vpc" {
  command = plan
  variables { vpc_cidr = "10.0.0.0/19" }
  assert {
    condition     = length(output.network_inventory.public_subnets) == 2 && alltrue([for c in values(output.network_inventory.public_subnet_cidrs) : endswith(c, "/27")])
    error_message = "A /19 VPC must still plan two public /27 subnets."
  }
}

run "bounded_runtime_roles" {
  command = plan
  variables {
    runtime_permissions_boundary_arn = "arn:aws:iam::123456789012:policy/miles/bootstrap/MilesDevRuntimeBoundary"
  }
}

run "reject_foreign_runtime_boundary" {
  command = plan
  variables {
    runtime_permissions_boundary_arn = "arn:aws:iam::111122223333:policy/miles/bootstrap/MilesDevRuntimeBoundary"
  }
  expect_failures = [var.runtime_permissions_boundary_arn]
}
