# Quy trình implement một task

## 1. Chốt scope

Đọc task trên backlog hiện hành: checklist, tiêu chí nghiệm thu, dependency và phần bàn giao cho task sau. Đối chiếu code đã merge; chỉ rõ phần làm được ngay và phần thực sự còn thiếu.

## 2. Tạo branch và worktree riêng

Mỗi task code dùng một branch và một worktree. Repo gốc ở `/home/ubuntu/miles`; worktree triển khai đặt trong `/home/ubuntu/miles-worktrees/`, tránh `/tmp` cho công việc dài hạn. Worktree có code/thay đổi riêng, dùng chung dữ liệu Git với repo gốc.

Ví dụ task thêm giới hạn bucket; thay `bucket-limits` bằng tên tính năng thực tế:

```bash
task_slug=bucket-limits
task_branch="feat/$task_slug"  # Sửa lỗi thì dùng fix/...
task_dir="/home/ubuntu/miles-worktrees/$task_slug"

git -C /home/ubuntu/miles status --short
git -C /home/ubuntu/miles worktree list
git -C /home/ubuntu/miles fetch origin main
mkdir -p /home/ubuntu/miles-worktrees
git -C /home/ubuntu/miles worktree add -b "$task_branch" "$task_dir" origin/main
```

Sau khi tạo worktree, không dùng `cd`. Với Codex, truyền đường dẫn tuyệt đối của worktree vào `workdir` cho từng lệnh shell. Điền giá trị thực, không truyền nguyên chuỗi `$task_dir`: tham số tool không tự mở rộng biến shell.

Ví dụ tham số cho `exec_command` của Codex:

```json
{"cmd":"git status --short","workdir":"/home/ubuntu/miles-worktrees/bucket-limits"}
```

Với DSH, dùng tham số thư mục theo schema; nếu có tool `working_directory`, có thể gọi một lần để đặt cwd cho cả session. Không mặc định tool giữ cwd hoặc biến shell giữa các lần gọi.

Nếu task đã có worktree thì dùng lại. Đặt tên theo chức năng, không dùng `codex/` hoặc `w1`, `w2`. Giữ nguyên thay đổi ở repo gốc; mọi thao tác code/commit/test thực hiện trong worktree task.

## 3. Implement và chia commit

Đọc luồng gọi và contract trước khi sửa; tái sử dụng implementation hiện có. Sửa đủ caller, test, fixture, config và tài liệu bị ảnh hưởng, trong đúng scope.

Stage đúng các file của từng phần hoàn chỉnh rồi commit riêng, ví dụ `feat(runtime): wire local storage`, `fix(cli): handle missing resources`. Không gom toàn task vào một commit. Ghi rõ ảnh hưởng downstream; thay đổi quyền, dữ liệu hoặc chi phí cần đủ ủy quyền trước khi thực hiện.

## 4. Kiểm thử và mở PR

Chạy test trực tiếp cho hành vi vừa sửa, rồi gate phù hợp như `make check`; thêm race hoặc `make test-local` khi thay đổi liên quan. Ghi đúng kiểm tra đã chạy, chưa chạy và giới hạn; local/mock không thay bằng chứng AWS thật.

Các lệnh dưới đây dùng `workdir` của worktree task:

```bash
git diff --check
git push -u origin HEAD
```

Mở PR vào `main`, ghi task ID, scope/checklist, bằng chứng và downstream. Tự rà diff trước khi gửi review; kiểm tra CI trên đúng head. Sau review, sửa và commit nối vào cùng branch/PR. Nếu cập nhật từ `main` gây conflict, giải quyết trong worktree task và chạy lại kiểm tra liên quan; không tự force-push.

## 5. Nghiệm thu và dọn

Chỉ approve/merge khi được giao và đủ điều kiện; admin bypass cần cho phép riêng. Cập nhật backlog với PR/bằng chứng; chỉ Done khi tiêu chí nghiệm thu thực sự đạt, không chỉ vì PR đã merge.

Sau merge, khi mọi commit đã push, worktree sạch và không còn công việc/process cần giữ, chạy lệnh dọn với `workdir="/home/ubuntu/miles"` và điền lại `task_dir` bằng đường dẫn worktree cần dọn:

```bash
task_dir="/home/ubuntu/miles-worktrees/bucket-limits"  # Thay bằng worktree cần dọn.
git fetch origin main
git -C "$task_dir" status --short
git worktree remove "$task_dir"
```

Nếu còn thay đổi chưa commit thì giữ lại và báo rõ; không dùng `--force` để dọn. Branch được giữ để tra cứu, xóa riêng khi không còn cần.
