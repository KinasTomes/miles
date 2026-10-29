mock_provider "aws" {
  mock_data "aws_ami" {
    defaults = { id = "ami-0123456789abcdef0", root_device_name = "/dev/xvda" }
  }
}
run "private_immutable_launch" {
  command = plan
  module { source = "../../modules/fleet" }
  variables {
    enabled                 = true
    name                    = "miles-dev"
    account_id              = "123456789012"
    region                  = "ap-southeast-1"
    ami_id                  = "ami-0123456789abcdef0"
    instance_type           = "t4g.small"
    architecture            = "arm64"
    subnet_ids              = ["subnet-0123456789abcdef0", "subnet-0123456789abcdef1"]
    security_group_id       = "sg-0123456789abcdef0"
    instance_profile        = "miles-dev-gateway"
    service_linked_role_arn = "arn:aws:iam::123456789012:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling_miles-dev"
    kms_key_arn             = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001"
    target_group_arn        = "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/miles-dev-s3/0123456789abcdef"
    data_bucket_name        = "miles-dev-data-123456789012-ap-southeast-1"
    release_sha256          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    release_version_id      = "fixture-version-1"
  }
  assert {
    condition     = !aws_launch_template.gateway[0].network_interfaces[0].associate_public_ip_address && aws_launch_template.gateway[0].metadata_options[0].http_tokens == "required" && aws_launch_template.gateway[0].metadata_options[0].http_put_response_hop_limit == 1 && aws_launch_template.gateway[0].block_device_mappings[0].ebs[0].encrypted && aws_launch_template.gateway[0].block_device_mappings[0].ebs[0].delete_on_termination
    error_message = "Private native services require IMDSv2 and encrypted disposable root disks."
  }
  assert {
    condition     = aws_autoscaling_group.gateway[0].health_check_type == "ELB" && !aws_autoscaling_group.gateway[0].force_delete && one(aws_autoscaling_group.gateway[0].initial_lifecycle_hook).heartbeat_timeout == 300 && length(base64decode(aws_launch_template.gateway[0].user_data)) < 16384 && strcontains(base64decode(aws_launch_template.gateway[0].user_data), "--version-id") && strcontains(base64decode(aws_launch_template.gateway[0].user_data), "release checksum mismatch")
    error_message = "Bootstrap must pin and verify the release; normal ASG deletion must retain its drain hook."
  }
}
