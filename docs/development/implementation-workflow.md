# Quy trình implement một task

Áp dụng cho làm việc local và môi trường Cloud chỉ có bản clone repository. Nội dung giao việc cần có trong prompt hoặc issue; tài liệu kỹ thuật liên quan cần có trên branch được sử dụng.

## 1. Nhận đủ thông tin trước khi làm

Mỗi task cần có:

- **Mục tiêu và checklist:** hành vi cần đạt, tiêu chí nghiệm thu và phần ngoài scope.
- **Dependency:** đầu ra cần dùng, PR/commit đã merge hoặc còn thiếu; ghi thời điểm xác nhận trạng thái.
- **Quyết định đã chốt:** contract, giới hạn, phần bàn giao và ảnh hưởng downstream.
- **Nguồn tham chiếu:** đường dẫn API, kiến trúc, contract và hướng dẫn kiểm thử trong repo.
- **Kiểm thử và quyền thực hiện:** bằng chứng local hay môi trường thật, phần được phép chạy, chi phí và cleanup nếu có.
- **Đầu ra:** code, test, tài liệu, các commit riêng và PR để review.

Đối chiếu thông tin giao việc với code thực tế. Nếu thiếu dữ kiện ảnh hưởng contract, quyền, dữ liệu hoặc nghiệm thu, báo rõ câu hỏi cần chốt; tiếp tục phần độc lập đã đủ thông tin. Không suy trạng thái hiện hành từ tài liệu kế hoạch cũ.

## 2. Chuẩn bị branch và nơi làm việc

Đọc `AGENTS.md` nếu có, `CONTRIBUTING.md` và hướng dẫn liên quan. Kiểm tra working tree; giữ nguyên thay đổi có sẵn. Dùng base đã chứa dependency cần thiết, mặc định là `origin/main` mới nhất.

Mỗi task dùng branch riêng, tên theo chức năng: `feat/...`, `fix/...` hoặc `docs/...`; không dùng tiền tố `codex/` hay mã tuần/task.

**Local dùng worktree riêng**, đặt cạnh repo trong thư mục `<tên-repo>-worktrees/`. Ví dụ thêm giới hạn bucket; thay tên tính năng cho phù hợp:

```bash
repo_root="$(git rev-parse --show-toplevel)"
worktree_dir="$(dirname "$repo_root")/$(basename "$repo_root")-worktrees/bucket-limits"
git -C "$repo_root" status --short
git -C "$repo_root" fetch origin main
mkdir -p "$(dirname "$worktree_dir")"
git -C "$repo_root" worktree add -b feat/bucket-limits "$worktree_dir" origin/main
```

Thực hiện mọi edit/test/commit trong worktree vừa tạo; truyền đúng thư mục làm việc cho từng lệnh của agent. Nếu task đã có worktree, dùng lại.

**Cloud đã có checkout riêng:** dùng branch task mà môi trường đã chuẩn bị. Nếu chưa có, tạo branch mô tả chức năng từ base phù hợp; không cần thêm worktree chỉ để lặp lại sự cô lập sẵn có. Không giả định đường dẫn hay công cụ của máy khác tồn tại.

## 3. Implement và chia commit

- Đọc luồng gọi, contract và test hiện có trước khi sửa. Tái sử dụng implementation phù hợp tại lớp chịu trách nhiệm.
- Sửa đủ caller, implementation, test, fixture, config và tài liệu bị ảnh hưởng, trong scope đã giao.
- Giữ validation, authorization, fencing, xử lý lỗi và tương thích. Báo rõ thay đổi contract hoặc tác động downstream trước khi mở rộng phạm vi.
- Chia commit theo từng phần hoàn chỉnh có ý nghĩa; stage đúng file, không gom toàn task thành một commit lớn hoặc đưa thay đổi không liên quan vào PR.
- Tên file và tài liệu kỹ thuật mô tả chức năng. Giữ mã task, phân công và trạng thái công việc trong prompt/issue/PR.

Khi làm song song, thống nhất ownership của phần dùng chung. Không thay node, drain hoặc cleanup môi trường mà lượt kiểm thử khác đang sử dụng.

## 4. Kiểm thử và tự review

Chạy kiểm tra nhỏ nhất chứng minh hành vi vừa sửa, gồm đường lỗi/boundary liên quan. Với code, chạy gate phù hợp như `make check`; dùng race hoặc `make test-local` khi thay đổi cần bằng chứng đó. Với tài liệu, kiểm tra diff, liên kết và quy tắc tên file.

Ghi rõ command, commit/build, môi trường, kết quả và giới hạn. Thiếu công cụ hoặc quyền thì báo phần chưa chạy; không coi mock/local là bằng chứng môi trường thật. Không tạo tài nguyên tính phí hoặc xóa dữ liệu khi chưa có phạm vi cho phép cụ thể.

Tự rà diff theo checklist và downstream; không thêm tính năng ngoài scope để đạt một bài test. Chạy `git diff --check` trước commit.

## 5. Bàn giao qua PR

Push branch và tạo PR vào base đã thống nhất. Mô tả cần có:

- Mục tiêu, scope và checklist đã hoàn thành/còn thiếu.
- Thay đổi contract hoặc ảnh hưởng downstream, nếu có.
- Bằng chứng kiểm thử gắn với commit; giới hạn và bước nghiệm thu thật còn lại.

Không đưa secret/token hoặc dữ liệu riêng vào code, lệnh mẫu, log hay PR. Sau review, sửa bằng commit nối vào cùng branch; kiểm tra lại phần bị ảnh hưởng và CI trên head mới. Khi cập nhật base có conflict, giải quyết và kiểm thử lại; không tự force-push.

## 6. Nghiệm thu và kết thúc

Chỉ approve/merge khi được giao quyền; admin bypass cần cho phép riêng cho PR đó. Xác nhận merge thực tế. Task chỉ được coi hoàn thành khi acceptance criteria có bằng chứng phù hợp; merged không tự thay cho nghiệm thu.

Bàn giao PR, kết quả và phần còn thiếu để cập nhật nơi quản lý task. Với môi trường thật, ghi lại cleanup và tài nguyên còn giữ. Chỉ dọn worktree local sau khi mọi commit đã push, working tree sạch và không còn process/công việc cần giữ; không dùng `--force` để dọn.
