mock_provider "aws" {
  mock_data "aws_ami" {
    defaults = { id = "ami-0123456789abcdef0" }
  }
}

variables {
  account_id = "123456789012"
}

run "legacy_names" {
  command = plan
  assert {
    condition     = output.runtime_inventory.data_bucket_name == "miles-dev-data-123456789012-ap-southeast-1" && output.session_configuration.runtime_iam_path == "/miles/dev/" && output.session_configuration.owner == "Hung"
    error_message = "Existing installation names and defaults must remain stable."
  }
}

run "member_installation" {
  command = plan
  variables {
    account_id                       = "210987654321"
    region                           = "ap-southeast-2"
    installation_name                = "miles-phong-test"
    environment                      = "test"
    owner                            = "Phong"
    runtime_iam_path                 = "/miles/phong/"
    edge_certificate_common_name     = "phong-edge"
    runtime_permissions_boundary_arn = "arn:aws:iam::210987654321:policy/miles/phong/RuntimeBoundary"
    enable_compute                   = true
    paused                           = false
    egress_mode                      = "interface"
    instance_type                    = "m6i.large"
    release_architecture             = "amd64"
    ami_id                           = "ami-0123456789abcdef0"
  }
  assert {
    condition     = output.runtime_inventory.data_bucket_name == "miles-phong-test-data-210987654321-ap-southeast-2" && output.runtime_inventory.compute.instance_type == "m6i.large" && length(output.runtime_inventory.expected_tables) == 20 && alltrue([for t in output.runtime_inventory.expected_tables : startswith(t, "miles-phong-test-")])
    error_message = "Account, Region, instance type and installation inventory must follow member inputs."
  }
  assert {
    condition     = output.session_configuration.runtime_iam_path == "/miles/phong/" && output.session_configuration.edge_certificate_common_name == "phong-edge" && keys(output.network_inventory.private_subnets) == ["ap-southeast-2a", "ap-southeast-2b"]
    error_message = "IAM identity and default AZs must follow the selected installation."
  }
}

run "wrong_boundary_account" {
  command = plan
  variables {
    runtime_permissions_boundary_arn = "arn:aws:iam::210987654321:policy/OtherAccount"
  }
  expect_failures = [var.runtime_permissions_boundary_arn]
}

run "wrong_certificate_region" {
  command = plan
  variables {
    certificate_arn = "arn:aws:acm:ap-southeast-2:123456789012:certificate/00000000-0000-0000-0000-000000000000"
  }
  expect_failures = [var.certificate_arn]
}

run "unsafe_name" {
  command = plan
  variables {
    installation_name = "miles-*"
  }
  expect_failures = [var.installation_name]
}

run "member_fleet" {
  command = plan
  variables {
    region                           = "ap-southeast-2"
    installation_name                = "miles-hai-test"
    owner                            = "Hai"
    environment                      = "test"
    paused                           = false
    enable_asg                       = true
    enable_alb                       = true
    egress_mode                      = "interface"
    ami_id                           = "ami-0123456789abcdef0"
    fleet_instance_type              = "t4g.small"
    runtime_permissions_boundary_arn = "arn:aws:iam::123456789012:policy/miles/hai/Boundary"
    certificate_arn                  = "arn:aws:acm:ap-southeast-2:123456789012:certificate/00000000-0000-0000-0000-000000000000"
    client_cidrs                     = ["192.0.2.1/32"]
    release_sha256                   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    release_version_id               = "fixture-version-1"
    runtime_iam_path                 = "/miles/hai/"
    edge_ca_pem                      = "fixture-public-ca"
    edge_certificate_common_name     = "hai-edge"
  }
  assert {
    condition     = output.fleet_contract.asg_name == "miles-hai-test-gateway" && output.edge_provider.certificate_common_name == "hai-edge"
    error_message = "Cloud fleet and Edge credentials must use the selected installation."
  }
}
