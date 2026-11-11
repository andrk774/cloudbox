import 'dart:io';

import 'package:cloudbox/lock.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  late Directory dir;
  setUp(() async => dir = await Directory.systemTemp.createTemp('cb-lock'));
  tearDown(() async => dir.delete(recursive: true));

  test('второй участник не берёт занятую блокировку, после освобождения — берёт', () async {
    final app = FileLock('${dir.path}/l'), bg = FileLock('${dir.path}/l');
    expect(await app.acquire(), isTrue);
    expect(await bg.acquire(), isFalse, reason: 'фоновая задача не должна грузить одновременно с приложением');
    await app.release();
    expect(await bg.acquire(), isTrue);
    await bg.release();
  });

  test('замок от убитого процесса считается устаревшим', () async {
    final f = File('${dir.path}/l');
    await f.writeAsString('${DateTime.now().subtract(const Duration(hours: 1)).millisecondsSinceEpoch}');
    expect(await FileLock(f.path).acquire(), isTrue);
  });

  test('свежий чужой замок не отбирается', () async {
    final f = File('${dir.path}/l');
    await f.writeAsString('${DateTime.now().millisecondsSinceEpoch}');
    expect(await FileLock(f.path).acquire(), isFalse);
  });

  test('из одновременных попыток побеждает ровно одна', () async {
    final results = await Future.wait(List.generate(20, (_) => FileLock('${dir.path}/l').acquire()));
    expect(results.where((r) => r).length, 1);
  });
}
